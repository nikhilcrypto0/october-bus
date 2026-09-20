package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/october-dev/october-bus/bus"
)

func connectionFixture(t *testing.T) (string, managedMCPConnection) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("managed remote connection files require Unix permissions")
	}
	root := t.TempDir()
	requireNoError(t, os.Chmod(root, 0o700))
	connection := managedMCPConnection{
		Version: 1, Endpoint: "http://127.0.0.1:12345/mcp", ScopeID: "workspace", AgentID: "node", ExecutionID: "run-1",
		AgentToken: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)), ExpiresAt: time.Now().Add(time.Hour),
	}
	path := filepath.Join(root, "connection.json")
	saveTestConnection(t, path, connection)
	return path, connection
}

func saveTestConnection(t *testing.T, path string, connection managedMCPConnection) {
	t.Helper()
	data, err := json.Marshal(connection)
	requireNoError(t, err)
	requireNoError(t, os.WriteFile(path+".tmp", data, 0o600))
	requireNoError(t, os.Rename(path+".tmp", path))
}

type connectionRoundTrip func(*http.Request) (*http.Response, error)

func (trip connectionRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return trip(request)
}

func TestManagedMCPConnectionRefreshAndRetirement(t *testing.T) {
	path, original := connectionFixture(t)
	expected := original.AgentToken
	expectedHost := "127.0.0.1:12345"
	calls := 0
	transport := agentTokenTransport{
		endpoint: original.Endpoint, connectionSource: managedMCPConnectionSource(path, original),
		base: connectionRoundTrip(func(request *http.Request) (*http.Response, error) {
			calls++
			require(t, request.Header.Get("Authorization") == "Bearer "+expected, "request used stale credentials")
			require(t, request.Host == expectedHost, "request used stale tunnel authority port")
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}"))}, nil
		}),
	}
	request, err := http.NewRequest(http.MethodPost, original.Endpoint, nil)
	requireNoError(t, err)
	for i := 0; i < 2; i++ {
		response, err := transport.RoundTrip(request)
		requireNoError(t, err)
		response.Body.Close()
		require(t, request.Header.Get("Authorization") == "", "mutated caller headers")
		renewed := original
		expected = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))
		renewed.AgentToken = expected
		expectedHost, renewed.HTTPHost = "127.0.0.1:23456", "127.0.0.1:23456"
		saveTestConnection(t, path, renewed)
	}
	for _, mutate := range []func(*managedMCPConnection){
		func(c *managedMCPConnection) { c.ExecutionID = "run-2" },
		func(c *managedMCPConnection) { c.ScopeID = "another-workspace" },
		func(c *managedMCPConnection) { c.AgentID = "another-node" },
		func(c *managedMCPConnection) { c.Endpoint = "https://another.example/mcp" },
		func(c *managedMCPConnection) { c.ExpiresAt = time.Now().Add(-time.Second) },
	} {
		changed := original
		mutate(&changed)
		saveTestConnection(t, path, changed)
		_, err := transport.RoundTrip(request)
		require(t, err != nil && calls == 2, "sent request after execution was replaced or expired")
	}
	requireNoError(t, os.Remove(path))
	_, err = transport.RoundTrip(request)
	require(t, err != nil && calls == 2, "fell back to old credentials after removal")
	saveTestConnection(t, path, original)
	request.URL.Path = "/another-endpoint"
	_, err = transport.RoundTrip(request)
	require(t, err != nil && calls == 2, "leaked credential to another endpoint")
}

func TestManagedMCPConnectionRejectsUnsafeFiles(t *testing.T) {
	for _, name := range []string{"public-file", "public-directory", "symlink", "malformed", "oversized", "unknown-field", "http-remote", "http-dns", "userinfo", "query", "token", "version", "host-remote", "host-https", "host-port"} {
		t.Run(name, func(t *testing.T) {
			path, connection := connectionFixture(t)
			switch name {
			case "public-file":
				requireNoError(t, os.Chmod(path, 0o644))
			case "public-directory":
				requireNoError(t, os.Chmod(filepath.Dir(path), 0o755))
			case "symlink":
				requireNoError(t, os.Rename(path, path+".real"))
				requireNoError(t, os.Symlink(path+".real", path))
			case "malformed":
				requireNoError(t, os.WriteFile(path, []byte("{broken"), 0o600))
			case "oversized":
				requireNoError(t, os.WriteFile(path, bytes.Repeat([]byte{' '}, 17*1024), 0o600))
			case "unknown-field":
				requireNoError(t, os.WriteFile(path, []byte(`{"adminToken":"never-accepted"}`), 0o600))
			default:
				switch name {
				case "http-remote":
					connection.Endpoint = "http://192.0.2.1/mcp"
				case "http-dns":
					connection.Endpoint = "http://localhost/mcp"
				case "userinfo":
					connection.Endpoint = "https://secret@example.com/mcp"
				case "query":
					connection.Endpoint = "https://example.com/mcp?token=secret"
				case "token":
					connection.AgentToken = "bad\r\nheader"
				case "version":
					connection.Version = 2
				case "host-remote":
					connection.HTTPHost = "192.0.2.1:12345"
				case "host-https":
					connection.Endpoint, connection.HTTPHost = "https://127.0.0.1/mcp", "127.0.0.1:12345"
				case "host-port":
					connection.HTTPHost = "127.0.0.1:65536"
				}
				saveTestConnection(t, path, connection)
			}
			_, err := readManagedMCPConnection(path)
			require(t, err != nil && !strings.Contains(err.Error(), connection.AgentToken), "unsafe file accepted or secret disclosed")
		})
	}
}

func TestMCPStdioManagedConnectionForwardsWithoutLocalDaemon(t *testing.T) {
	path, connection := connectionFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	address, _, senderToken, receiverToken, cleanup := startTestServer(t, ctx, "managed-connection")
	defer cleanup()
	connection.Endpoint, connection.AgentToken = address+"/mcp", senderToken
	saveTestConnection(t, path, connection)
	stderr := new(syncBuffer)
	command := mcpBridgeCommand("", "", stderr)
	args, err := json.Marshal([]string{"--connection-file", path})
	requireNoError(t, err)
	command.Env = setEnvironment(command.Env, "OCTOBER_BUS_MCP_STDIO_TEST_ARGS", string(args))
	client := mcp.NewClient(&mcp.Implementation{Name: "managed-bridge-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: command}, nil)
	require(t, err == nil, "connect managed bridge: %v; %s", err, stderr.String())
	defer session.Close()
	callMCPBridgeTool(t, ctx, session, "message_peer", map[string]any{"peer": "receiver", "message": "managed connection"})
	messages, err := (bus.Client{Address: address, Token: receiverToken}).PullInbox(ctx, 10, 0)
	require(t, err == nil && len(messages) == 1 && messages[0].Body == "managed connection", "bridge did not deliver through existing authority")
	_, err = (bus.Client{Address: address, Token: receiverToken}).AcknowledgeMessages(ctx, []string{messages[0].ID})
	requireNoError(t, err)
	connection.AgentToken = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32))
	saveTestConnection(t, path, connection)
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_peers", Arguments: map[string]any{}})
	require(t, err != nil || result.IsError, "ignored renewed credential")
	connection.AgentToken = senderToken
	saveTestConnection(t, path, connection)
	callMCPBridgeTool(t, ctx, session, "list_peers", map[string]any{})
	connection.ExecutionID = "replacement"
	saveTestConnection(t, path, connection)
	result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "message_peer", Arguments: map[string]any{"peer": "receiver", "message": "must not send"}})
	require(t, err != nil || result.IsError, "old bridge used replacement execution")
	messages, err = (bus.Client{Address: address, Token: receiverToken}).PullInbox(ctx, 10, 0)
	require(t, err == nil && len(messages) == 0, "replacement message escaped")
}

func TestMCPStdioManagedConnectionDoesNotReplayLostMutation(t *testing.T) {
	path, connection := connectionFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	address, _, senderToken, receiverToken, cleanup := startTestServer(t, ctx, "managed-lost-reply")
	defer cleanup()
	var mutations atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error("could not read proxy request")
			return
		}
		request, err := http.NewRequestWithContext(r.Context(), r.Method, address+r.URL.RequestURI(), bytes.NewReader(body))
		if err != nil {
			t.Error("could not create proxy request")
			return
		}
		request.Header = r.Header.Clone()
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Error("proxy request failed")
			return
		}
		defer response.Body.Close()
		if bytes.Contains(body, []byte(`"message_peer"`)) && mutations.Add(1) == 1 {
			// The authority has accepted the message; lose only its response.
			_, _ = io.Copy(io.Discard, response.Body)
			socket, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error("could not simulate lost response")
				return
			}
			_ = socket.Close()
			return
		}
		for name, values := range response.Header {
			w.Header()[name] = values
		}
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
	}))
	defer proxy.Close()
	connection.Endpoint, connection.AgentToken = proxy.URL+"/mcp", senderToken
	saveTestConnection(t, path, connection)
	stderr := new(syncBuffer)
	command := mcpBridgeCommand("", "", stderr)
	args, _ := json.Marshal([]string{"--connection-file", path})
	command.Env = setEnvironment(command.Env, "OCTOBER_BUS_MCP_STDIO_TEST_ARGS", string(args))
	client := mcp.NewClient(&mcp.Implementation{Name: "lost-reply-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: command}, nil)
	requireNoError(t, err)
	defer session.Close()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "message_peer", Arguments: map[string]any{"peer": "receiver", "message": "accepted once"}})
	require(t, err != nil || result.IsError, "lost response reported success")
	callMCPBridgeTool(t, ctx, session, "list_peers", map[string]any{})
	require(t, mutations.Load() == 1, "replayed mutation while reconnecting")
	messages, err := (bus.Client{Address: address, Token: receiverToken}).PullInbox(ctx, 10, 0)
	require(t, err == nil && len(messages) == 1 && messages[0].Body == "accepted once", "lost response duplicated or dropped committed message")
}
