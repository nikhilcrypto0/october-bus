package main

import (
	"context"
	"errors"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Managed workers can outlive their route/authority connection. The SDK closes
// its HTTP session on transport failures, so establish a new session for the NEXT
// tool call. Never replay the failed call: a mutation may already have committed.
type managedMCPUpstream struct {
	mu      sync.Mutex
	session *mcp.ClientSession
	connect func(context.Context) (*mcp.ClientSession, error)
	closed  bool
}

func (upstream *managedMCPUpstream) call(ctx context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	upstream.mu.Lock()
	if upstream.closed {
		upstream.mu.Unlock()
		return nil, errors.New("managed MCP bridge is closed")
	}
	if err := ctx.Err(); err != nil {
		upstream.mu.Unlock()
		return nil, err
	}
	if upstream.session == nil {
		session, err := upstream.connect(ctx)
		if err != nil {
			upstream.mu.Unlock()
			return nil, err
		}
		upstream.session = session
	}
	session := upstream.session
	upstream.mu.Unlock()
	result, err := session.CallTool(ctx, params)
	var protocolError *jsonrpc.Error
	if err != nil && ctx.Err() == nil && !errors.As(err, &protocolError) {
		upstream.mu.Lock()
		if upstream.session == session {
			upstream.session = nil
		}
		upstream.mu.Unlock()
		// Other calls on this failed connection may also fail. Each surfaces its
		// own outcome; none inherits permission to replay a possibly accepted tool.
		_ = session.Close()
	}
	return result, err
}

func (upstream *managedMCPUpstream) close() {
	upstream.mu.Lock()
	upstream.closed = true
	session := upstream.session
	upstream.session = nil
	upstream.mu.Unlock()
	if session != nil {
		_ = session.Close()
	}
}
