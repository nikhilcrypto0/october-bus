// Package gateway exposes an operator-provisioned MCP connector while keeping
// the daemon and its administrative routes on loopback.
package gateway

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/october-dev/october-bus/bus"
)

type ClientConfig struct {
	Scope        string   `json:"scope"`
	Agent        string   `json:"agent"`
	Name         string   `json:"name"`
	APIKeySHA256 string   `json:"apiKeySha256"`
	ConnectTo    []string `json:"connectTo,omitempty"`
}

type Config struct {
	PublicURL string         `json:"publicURL"`
	Clients   []ClientConfig `json:"clients"`
}

// Validate rejects ambiguous credentials and identities before any registration.
func (c Config) Validate() error {
	u, err := url.Parse(c.PublicURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return errors.New("publicURL must be an HTTPS origin without a path, credentials, query, or fragment")
	}
	if len(c.Clients) == 0 || len(c.Clients) > 128 {
		return errors.New("configure between 1 and 128 connector clients")
	}
	keys, identities := map[string]bool{}, map[string]bool{}
	for i, client := range c.Clients {
		for _, id := range append([]string{client.Scope, client.Agent}, client.ConnectTo...) {
			if _, err := bus.ScopeTokenPath("", id); err != nil {
				return fmt.Errorf("client %d has an invalid scope, agent, or peer ID", i)
			}
		}
		if strings.TrimSpace(client.Name) == "" || len(client.Name) > 256 || len(client.ConnectTo) > 128 {
			return fmt.Errorf("client %d has an invalid name or peer list", i)
		}
		digest, err := hex.DecodeString(client.APIKeySHA256)
		if err != nil || len(digest) != sha256.Size {
			return fmt.Errorf("client %d requires a SHA-256 API key digest", i)
		}
		key, identity := string(digest), client.Scope+"/"+client.Agent
		if keys[key] || identities[identity] {
			return errors.New("connector API keys and scope/agent pairs must be unique")
		}
		keys[key], identities[identity] = true, true
	}
	return nil
}

type client struct {
	digest  []byte
	session *bus.AgentSession
	budget  chan struct{}
}

type Gateway struct {
	origin   string
	host     string
	upstream string
	clients  []client
	proxy    *httputil.ReverseProxy
	agentMux *http.ServeMux
	budget   chan struct{}
	done     chan struct{}
	failOnce sync.Once
	failed   atomic.Int32
}

// New starts one execution per connector, not per HTTP request. The execution
// describes the gateway, never evidence that a remote model is awake or idle.
// The resolver only runs at startup; scope credentials are never sent to clients.
func New(ctx context.Context, upstream string, config Config, resolve func(string) (string, error)) (_ *Gateway, err error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	u, err := url.Parse(upstream)
	if err != nil {
		return nil, errors.New("invalid upstream address")
	}
	ip, err := netip.ParseAddr(u.Hostname())
	if err != nil || !ip.IsLoopback() || u.Scheme != "http" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("upstream must be a loopback HTTP origin")
	}
	public, _ := url.Parse(config.PublicURL)
	g := &Gateway{origin: config.PublicURL, host: public.Host, upstream: upstream, budget: make(chan struct{}, 128), done: make(chan struct{})}
	g.proxy = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(u)
			r.Out.Host = u.Host
			// Never trust or forward client-supplied proxy identity or cookies.
			r.Out.Header.Del("Forwarded")
			r.Out.Header.Del("Cookie")
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			failure(w, http.StatusBadGateway, "Bus is unavailable")
		},
	}
	defer func() {
		if err != nil {
			cleanup, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancel()
			_ = g.Close(cleanup)
		}
	}()
	// Resolve all credentials before registrations so an invalid later entry
	// cannot unnecessarily replace an earlier connector's active execution.
	tokens := make([]string, len(config.Clients))
	for i, entry := range config.Clients {
		tokens[i], err = resolve(entry.Scope)
		if err != nil {
			return nil, fmt.Errorf("resolve scope for client %d: %w", i, err)
		}
	}
	for i, entry := range config.Clients {
		session, startErr := bus.StartAgentSession(ctx, bus.AgentSessionOptions{
			Address: upstream, ScopeToken: tokens[i], HeartbeatInterval: 5 * time.Second,
			HTTP:         &http.Client{Timeout: 30 * time.Second, CheckRedirect: noRedirect},
			Registration: bus.RegisterAgentInput{ID: entry.Agent, DisplayName: entry.Name, ConnectTo: entry.ConnectTo, LeaseMS: 30_000},
		})
		if startErr != nil {
			return nil, fmt.Errorf("start connector %d: %w", i, startErr)
		}
		digest, _ := hex.DecodeString(entry.APIKeySHA256)
		g.clients = append(g.clients, client{digest: digest, session: session, budget: make(chan struct{}, 16)})
		go func() {
			<-session.Done()
			// One scope owner can replace its connector execution. That must
			// not take unrelated scopes offline. Exit only when all have ended.
			if g.failed.Add(1) == int32(len(config.Clients)) {
				g.failOnce.Do(func() { close(g.done) })
			}
		}()
	}
	g.agentMux = g.remoteAgentRoutes()
	return g, nil
}

func noRedirect(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }

func (g *Gateway) Done() <-chan struct{} { return g.done }

func (g *Gateway) Close(ctx context.Context) error {
	g.failOnce.Do(func() { close(g.done) })
	var result error
	for _, c := range g.clients {
		result = errors.Join(result, c.session.Close(ctx))
	}
	return result
}

func failure(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Host != g.host || (r.Header.Get("Origin") != "" && r.Header.Get("Origin") != g.origin) {
		failure(w, http.StatusForbidden, "Host or Origin is not allowed")
		return
	}
	// Reject path aliases before routing. The private daemon must never see an
	// encoded slash, cleaned traversal, or redirect to an administrative route.
	if path.Clean(r.URL.Path) != r.URL.Path || strings.Contains(r.URL.EscapedPath(), "%") || strings.Contains(r.URL.Path, "\\") {
		failure(w, http.StatusNotFound, "Route not found")
		return
	}
	select {
	case g.budget <- struct{}{}:
		defer func() { <-g.budget }()
	default:
		w.Header().Set("Retry-After", "1")
		failure(w, http.StatusTooManyRequests, "Concurrent request limit reached")
		return
	}
	if r.URL.Path == "/health/live" && r.Method == http.MethodGet {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	select {
	case <-g.done:
		failure(w, http.StatusServiceUnavailable, "Connector is unavailable")
		return
	default:
	}
	if r.URL.Path == "/health/ready" && r.Method == http.MethodGet {
		for _, c := range g.clients {
			select {
			case <-c.session.Done():
				failure(w, http.StatusServiceUnavailable, "A connector is unavailable")
				return
			default:
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if _, err := (bus.Client{Address: g.upstream, HTTP: &http.Client{CheckRedirect: noRedirect}}).Health(ctx); err != nil {
			failure(w, http.StatusServiceUnavailable, "Bus is unavailable")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.URL.Path == "/mcp" {
		g.serveConnector(w, r)
		return
	}
	g.agentMux.ServeHTTP(w, r)
}

func (g *Gateway) serveConnector(w http.ResponseWriter, r *http.Request) {
	scheme, key, found := strings.Cut(r.Header.Get("Authorization"), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || len(key) < 32 || len(key) > 256 || len(r.Header.Values("Authorization")) != 1 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="october-bus"`)
		failure(w, http.StatusUnauthorized, "Invalid connector API key")
		return
	}
	digest := sha256.Sum256([]byte(key))
	for _, c := range g.clients {
		if subtle.ConstantTimeCompare(digest[:], c.digest) != 1 {
			continue
		}
		select {
		case <-c.session.Done():
			failure(w, http.StatusServiceUnavailable, "Connector is unavailable")
			return
		default:
		}
		if r.Method != http.MethodPost && r.Method != http.MethodGet && r.Method != http.MethodDelete {
			w.Header().Set("Allow", "POST, GET, DELETE")
			failure(w, http.StatusMethodNotAllowed, "Method not allowed")
			return
		}
		select {
		case c.budget <- struct{}{}:
			defer func() { <-c.budget }()
		default:
			w.Header().Set("Retry-After", "1")
			failure(w, http.StatusTooManyRequests, "Connector request limit reached")
			return
		}
		clone := r.Clone(r.Context())
		clone.Header.Set("Authorization", "Bearer "+c.session.Registration.AgentToken)
		g.proxy.ServeHTTP(w, clone)
		return
	}
	w.Header().Set("WWW-Authenticate", `Bearer realm="october-bus"`)
	failure(w, http.StatusUnauthorized, "Invalid connector API key")
}

// Remote launchers authenticate directly to the existing Bus agent/scope APIs.
// This is an explicit allowlist: new daemon/admin APIs do not become public.
func (g *Gateway) remoteAgentRoutes() *http.ServeMux {
	mux := http.NewServeMux()
	patterns := []string{
		"GET /health", "GET /health/live", "GET /health/ready",
		"POST /mcp", "GET /mcp", "DELETE /mcp",
		"GET /v1/agents", "POST /v1/agents", "POST /v1/links",
		"GET /v1/me", "PATCH /v1/me/heartbeat", "POST /v1/me/retire", "GET /v1/peers",
		"POST /v1/messages", "GET /v1/messages/{messageId}", "POST /v1/messages/ack",
		"POST /v1/inbox/reserve", "POST /v1/inbox/{reservationId}/commit", "POST /v1/inbox/{reservationId}/release",
		"GET /v1/tasks", "POST /v1/tasks", "GET /v1/tasks/page",
		"POST /v1/tasks/{taskId}/claim", "POST /v1/tasks/{taskId}/release", "POST /v1/tasks/{taskId}/complete",
		"GET /v1/tasks/{taskId}/progress", "POST /v1/tasks/{taskId}/progress",
		"POST /v1/escalations", "GET /v1/escalations/{escalationId}",
		"GET /v1/scope/escalations", "POST /v1/scope/escalations/{escalationId}/resolve", "GET /v1/events",
		"POST /outputs/{streamId}/values",
	}
	for _, pattern := range patterns {
		method, route, _ := strings.Cut(pattern, " ")
		mux.HandleFunc(method+" /bus"+route, func(w http.ResponseWriter, r *http.Request) {
			clone := r.Clone(r.Context())
			clone.URL.Path = strings.TrimPrefix(clone.URL.Path, "/bus")
			g.proxy.ServeHTTP(w, clone)
		})
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { failure(w, http.StatusNotFound, "Route not found") })
	return mux
}
