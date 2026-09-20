package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// A host controller owns this file and renews it by atomic rename. The bridge
// receives only an execution credential, never scope or administrator authority.
// These labels pin the connection; the server still authenticates the token.
type managedMCPConnection struct {
	Version     int       `json:"version"`
	Endpoint    string    `json:"endpoint"`
	HTTPHost    string    `json:"httpHost,omitempty"`
	ScopeID     string    `json:"scopeId"`
	AgentID     string    `json:"agentId"`
	ExecutionID string    `json:"executionId"`
	AgentToken  string    `json:"agentToken"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

func readManagedMCPConnection(path string) (managedMCPConnection, error) {
	var connection managedMCPConnection
	invalid := errors.New("managed MCP connection is missing, expired, or invalid; reconnect through its host controller")
	// Remote workers currently run on Unix. Do not pretend POSIX bits check a
	// Windows ACL; the existing environment/local-discovery modes remain available.
	if runtime.GOOS == "windows" || !filepath.IsAbs(path) {
		return connection, errors.New("managed MCP connections require an absolute path with Unix owner-only permissions")
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil || !parent.IsDir() || parent.Mode().Perm()&0o077 != 0 {
		return connection, invalid
	}
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm()&0o077 != 0 || before.Size() > 16*1024 {
		return connection, invalid
	}
	file, err := os.Open(path)
	if err != nil {
		return connection, invalid
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || !after.Mode().IsRegular() || after.Mode().Perm()&0o077 != 0 {
		return connection, invalid
	}
	data, err := io.ReadAll(io.LimitReader(file, 16*1024+1))
	if err != nil || len(data) > 16*1024 {
		return connection, invalid
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&connection) != nil || decoder.Decode(new(any)) != io.EOF {
		return managedMCPConnection{}, invalid
	}
	if connection.Version != 1 || !connection.ExpiresAt.After(time.Now()) {
		return managedMCPConnection{}, invalid
	}
	for _, id := range []string{connection.ScopeID, connection.AgentID, connection.ExecutionID} {
		if id == "" || len(id) > 256 || strings.ContainsAny(id, "\x00\r\n") {
			return managedMCPConnection{}, invalid
		}
	}
	token, err := base64.RawURLEncoding.DecodeString(connection.AgentToken)
	if err != nil || len(token) != 32 || base64.RawURLEncoding.EncodeToString(token) != connection.AgentToken {
		return managedMCPConnection{}, invalid
	}
	endpoint, err := url.Parse(connection.Endpoint)
	if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return managedMCPConnection{}, invalid
	}
	loopback := net.ParseIP(endpoint.Hostname())
	if endpoint.Scheme != "https" && !(endpoint.Scheme == "http" && loopback != nil && loopback.IsLoopback()) {
		return managedMCPConnection{}, invalid
	}
	if connection.HTTPHost != "" {
		host, port, err := net.SplitHostPort(connection.HTTPHost)
		portNumber, portErr := strconv.Atoi(port)
		// A TCP tunnel preserves HTTP Host. Permit only its loopback authority
		// port to differ; no arbitrary virtual-host or HTTPS routing override.
		if err != nil || portErr != nil || portNumber < 1 || portNumber > 65535 || endpoint.Scheme != "http" || host != endpoint.Hostname() {
			return managedMCPConnection{}, invalid
		}
	}
	return connection, nil
}

func managedMCPConnectionSource(path string, original managedMCPConnection) func() (managedMCPConnection, error) {
	return func() (managedMCPConnection, error) {
		current, err := readManagedMCPConnection(path)
		if err != nil {
			return managedMCPConnection{}, err
		}
		if current.Endpoint != original.Endpoint || current.ScopeID != original.ScopeID || current.AgentID != original.AgentID || current.ExecutionID != original.ExecutionID {
			return managedMCPConnection{}, errors.New("managed MCP execution changed; start a new bridge through its host controller")
		}
		return current, nil
	}
}

func refuseMCPRedirect(_ *http.Request, _ []*http.Request) error {
	return errors.New("MCP endpoint redirected; reconnect through its host controller")
}
