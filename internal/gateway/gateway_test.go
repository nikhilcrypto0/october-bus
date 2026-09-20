package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/october-dev/october-bus/bus"
)

type testTransport struct{ key string }

func (tr testTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Host = "bus.example.test"
	if tr.key != "" {
		clone.Header.Set("Authorization", "Bearer "+tr.key)
	}
	return http.DefaultTransport.RoundTrip(clone)
}

type fixture struct {
	runtime *bus.Runtime
	private *httptest.Server
	public  *httptest.Server
	g       *Gateway
	config  Config
	scopes  map[string]string
	keys    []string
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func setup(t *testing.T, count int) *fixture {
	t.Helper()
	runtime, err := bus.Open(":memory:")
	must(t, err)
	t.Cleanup(func() { runtime.Close() })
	f := &fixture{runtime: runtime, scopes: map[string]string{}, config: Config{PublicURL: "https://bus.example.test"}}
	f.private = httptest.NewServer(bus.NewServer(runtime, bus.ServerOptions{AdminToken: "private-admin-token"}))
	t.Cleanup(f.private.Close)
	for i := 0; i < count; i++ {
		scope, err := runtime.CreateScope(context.Background(), bus.CreateScopeInput{ID: fmt.Sprintf("owner-%d", i)})
		must(t, err)
		f.scopes[scope.ScopeID] = scope.ScopeToken
		key := strings.Repeat(fmt.Sprint(i), 48)
		f.keys = append(f.keys, key)
		digest := sha256.Sum256([]byte(key))
		f.config.Clients = append(f.config.Clients, ClientConfig{Scope: scope.ScopeID, Agent: "muse", Name: "Muse connector", APIKeySHA256: hex.EncodeToString(digest[:])})
	}
	f.g, err = New(context.Background(), f.private.URL, f.config, func(scope string) (string, error) { return f.scopes[scope], nil })
	must(t, err)
	t.Cleanup(func() { f.g.Close(context.Background()) })
	f.public = httptest.NewServer(f.g)
	t.Cleanup(f.public.Close)
	return f
}

func (f *fixture) mcp(t *testing.T, key string) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "hosted-connector-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: f.public.URL + "/mcp", HTTPClient: &http.Client{Transport: testTransport{key: key}}, DisableStandaloneSSE: true, MaxRetries: -1,
	}, nil)
	must(t, err)
	t.Cleanup(func() { session.Close() })
	return session
}

func call[T any](t *testing.T, session *mcp.ClientSession, name string, arguments any) T {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: arguments})
	must(t, err)
	if result.IsError {
		t.Fatalf("%s failed: %+v", name, result.Content)
	}
	data, err := json.Marshal(result.StructuredContent)
	must(t, err)
	var value T
	must(t, json.Unmarshal(data, &value))
	return value
}

func (f *fixture) worker(t *testing.T, scope string) *bus.AgentSession {
	t.Helper()
	session, err := bus.StartAgentSession(context.Background(), bus.AgentSessionOptions{
		Address: f.public.URL + "/bus", ScopeToken: f.scopes[scope],
		HTTP:         &http.Client{Transport: testTransport{}, Timeout: 5 * time.Second},
		Registration: bus.RegisterAgentInput{ID: "worker", DisplayName: "Laptop agent", ConnectTo: []string{"muse"}},
	})
	must(t, err)
	t.Cleanup(func() { session.Close(context.Background()) })
	return session
}

func TestGatewayRoundTripAndScopeIsolation(t *testing.T) {
	f := setup(t, 2)
	worker := f.worker(t, "owner-0")
	other := f.worker(t, "owner-1")
	connector := f.mcp(t, f.keys[0])
	tools, err := connector.ListTools(context.Background(), &mcp.ListToolsParams{})
	must(t, err)
	if len(tools.Tools) != 15 {
		t.Fatalf("expected Bus tools, got %d", len(tools.Tools))
	}
	peers := call[struct{ Peers []bus.Agent }](t, connector, "list_peers", map[string]any{})
	if len(peers.Peers) != 1 || peers.Peers[0].ID != "worker" {
		t.Fatalf("wrong peers: %+v", peers)
	}
	arguments := map[string]any{"peer": "worker", "mode": "request", "message": "Review the checkout change", "idempotencyKey": "hosted-roundtrip"}
	receipt := call[bus.DeliveryReceipt](t, connector, "message_peer", arguments)
	retry := call[bus.DeliveryReceipt](t, connector, "message_peer", arguments)
	if receipt.MessageID == "" || receipt.MessageID != retry.MessageID {
		t.Fatal("retry created a different request")
	}
	messages, err := worker.Client.PullInbox(context.Background(), 10, 0)
	must(t, err)
	if len(messages) != 1 || messages[0].Body != "Review the checkout change" {
		t.Fatalf("laptop did not receive the request: %+v", messages)
	}
	foreign, err := other.Client.PullInbox(context.Background(), 10, 0)
	must(t, err)
	if len(foreign) != 0 {
		t.Fatal("request crossed scopes")
	}
	if _, err := other.Client.Receipt(context.Background(), receipt.MessageID); err == nil {
		t.Fatal("other scope read the receipt")
	}
	_, err = worker.Client.AcknowledgeMessages(context.Background(), []string{receipt.MessageID})
	must(t, err)
	_, err = worker.Client.SendMessage(context.Background(), bus.SendMessageInput{To: "muse", Body: "Review complete", Mode: bus.MessageResponse, ResponseTo: receipt.MessageID})
	must(t, err)
	replies := call[struct{ Messages []bus.Message }](t, connector, "check_inbox", map[string]any{})
	if len(replies.Messages) != 1 || replies.Messages[0].Body != "Review complete" {
		t.Fatalf("connector did not receive the reply: %+v", replies)
	}
	call[map[string]any](t, connector, "acknowledge_messages", map[string]any{"messageIds": []string{replies.Messages[0].ID}})
	final := call[bus.DeliveryReceipt](t, connector, "message_receipt", map[string]any{"messageId": receipt.MessageID})
	if final.ResponseMessageID != replies.Messages[0].ID {
		t.Fatal("reply was not correlated")
	}
}

func TestGatewayRejectsUntrustedRequestsAndPrivateRoutes(t *testing.T) {
	f := setup(t, 1)
	tests := []struct {
		name, method, path, key, host, origin string
		status                                int
	}{
		{"missing key", "POST", "/mcp", "", "bus.example.test", "", 401},
		{"wrong key", "POST", "/mcp", strings.Repeat("z", 48), "bus.example.test", "", 401},
		{"scope token is not connector key", "POST", "/mcp", f.scopes["owner-0"], "bus.example.test", "", 401},
		{"wrong host", "POST", "/mcp", f.keys[0], "evil.example", "", 403},
		{"wrong origin", "POST", "/mcp", f.keys[0], "bus.example.test", "https://evil.example", 403},
		{"query key is not auth", "POST", "/mcp?access_token=" + f.keys[0], "", "bus.example.test", "", 401},
		{"admin blocked even with admin token", "POST", "/bus/v1/admin/shutdown", "private-admin-token", "bus.example.test", "", 404},
		{"scope creation blocked", "POST", "/bus/v1/scopes", "private-admin-token", "bus.example.test", "", 404},
		{"backup blocked", "GET", "/bus/v1/admin/backup", "private-admin-token", "bus.example.test", "", 404},
		{"pruning blocked", "POST", "/bus/v1/scope/storage/prune", f.scopes["owner-0"], "bus.example.test", "", 404},
		{"traversal blocked", "POST", "/bus/v1/tasks/../admin/shutdown", "private-admin-token", "bus.example.test", "", 404},
		{"encoded slash blocked", "POST", "/bus/v1%2fadmin/shutdown", "private-admin-token", "bus.example.test", "", 404},
		{"connector key cannot register agents", "POST", "/bus/v1/agents", f.keys[0], "bus.example.test", "", 401},
		{"connector key cannot use raw MCP", "POST", "/bus/mcp", f.keys[0], "bus.example.test", "", 401},
		{"ready", "GET", "/health/ready", "", "bus.example.test", "", 204},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
			r.Host = tc.host
			if tc.key != "" {
				r.Header.Set("Authorization", "Bearer "+tc.key)
			}
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			f.g.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tc.status, w.Body.String())
			}
			if strings.Contains(w.Body.String(), f.keys[0]) || strings.Contains(w.Body.String(), f.scopes["owner-0"]) {
				t.Fatal("credential leaked in error response")
			}
		})
	}
}

func TestGatewayRestartPreservesQueuedWorkAndRotatesKey(t *testing.T) {
	f := setup(t, 1)
	worker := f.worker(t, "owner-0")
	_, err := worker.Client.SendMessage(context.Background(), bus.SendMessageInput{To: "muse", Mode: bus.MessageNotify, Body: "Durable result"})
	must(t, err)
	oldExecutionToken := f.g.clients[0].session.Registration.AgentToken
	must(t, f.g.Close(context.Background()))
	if _, err := (bus.Client{Address: f.private.URL, Token: oldExecutionToken}).ListPeers(context.Background()); err == nil {
		t.Fatal("closed connector retained execution authority")
	}
	newKey := strings.Repeat("n", 48)
	digest := sha256.Sum256([]byte(newKey))
	f.config.Clients[0].APIKeySHA256 = hex.EncodeToString(digest[:])
	restarted, err := New(context.Background(), f.private.URL, f.config, func(scope string) (string, error) { return f.scopes[scope], nil })
	must(t, err)
	t.Cleanup(func() { restarted.Close(context.Background()) })
	public := httptest.NewServer(restarted)
	t.Cleanup(public.Close)
	f.public = public
	r, _ := http.NewRequest("POST", public.URL+"/mcp", strings.NewReader(`{}`))
	response, err := (&http.Client{Transport: testTransport{key: f.keys[0]}}).Do(r)
	must(t, err)
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatal("revoked connector key still accepted")
	}
	connector := f.mcp(t, newKey)
	replies := call[struct{ Messages []bus.Message }](t, connector, "check_inbox", map[string]any{})
	if len(replies.Messages) != 1 || replies.Messages[0].Body != "Durable result" {
		t.Fatal("gateway restart lost accepted work")
	}
}

func TestGatewaySessionFailureFailsClosed(t *testing.T) {
	f := setup(t, 1)
	must(t, f.g.clients[0].session.Close(context.Background()))
	select {
	case <-f.g.Done():
	case <-time.After(time.Second):
		t.Fatal("execution loss did not signal gateway shutdown")
	}
	for _, route := range []string{"/mcp", "/health/ready", "/bus/v1/agents"} {
		r := httptest.NewRequest("GET", route, nil)
		r.Host = "bus.example.test"
		r.Header.Set("Authorization", "Bearer "+f.keys[0])
		w := httptest.NewRecorder()
		f.g.ServeHTTP(w, r)
		if w.Code != 503 {
			t.Fatalf("failed gateway served %s: %d", route, w.Code)
		}
	}
}

func TestGatewayOneFailedConnectorDoesNotDisableOtherScopes(t *testing.T) {
	f := setup(t, 2)
	must(t, f.g.clients[0].session.Close(context.Background()))
	connector := f.mcp(t, f.keys[1])
	call[map[string]any](t, connector, "get_node_status", map[string]any{})
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{}`))
	r.Host = "bus.example.test"
	r.Header.Set("Authorization", "Bearer "+f.keys[0])
	w := httptest.NewRecorder()
	f.g.ServeHTTP(w, r)
	if w.Code != 503 {
		t.Fatal("failed connector did not fail closed")
	}
	select {
	case <-f.g.Done():
		t.Fatal("one scope disabled other connectors")
	default:
	}
}

func TestGatewayValidatesConfigurationBeforeRegistration(t *testing.T) {
	f := setup(t, 1)
	for _, change := range []func(*Config){
		func(c *Config) { c.PublicURL = "http://bus.example.test" },
		func(c *Config) { c.PublicURL += "/mcp" },
		func(c *Config) { c.Clients = nil },
		func(c *Config) { c.Clients = append(c.Clients, c.Clients[0]) },
		func(c *Config) { c.Clients[0].APIKeySHA256 = "weak" },
		func(c *Config) { c.Clients[0].Agent = "../escape" },
	} {
		config := f.config
		config.Clients = append([]ClientConfig{}, f.config.Clients...)
		change(&config)
		if _, err := New(context.Background(), f.private.URL, config, func(string) (string, error) {
			t.Fatal("invalid config reached credential resolution")
			return "", nil
		}); err == nil {
			t.Fatal("invalid configuration accepted")
		}
	}
}
