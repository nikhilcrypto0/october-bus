# Host-managed MCP connections

October Desktop and other host controllers can run the native Bus stdio bridge
against their existing MCP authority. The bridge does not start another daemon,
create a scope, or require a copy of the controller's administrator credential.

```sh
october-bus mcp stdio --connection-file /absolute/private/connection.json
```

This mode is intended for managed Unix workers. The controller provisions a real
directory with mode `0700` and a regular file with mode `0600`. It writes a new
file in that directory and atomically renames it over the old one when renewing
the connection. The file has this shape (placeholders are not usable credentials):

```json
{
  "version": 1,
  "endpoint": "http://127.0.0.1:43120/mcp",
  "httpHost": "127.0.0.1:43121",
  "scopeId": "workspace-id",
  "agentId": "canvas-node-id",
  "executionId": "execution-id",
  "agentToken": "<execution credential minted by the authority>",
  "expiresAt": "<RFC3339 expiry chosen by the controller>"
}
```

The endpoint must use HTTPS or literal loopback HTTP. Redirects are refused. A
stable SSH reverse-forward port can provide the loopback route; an authenticated
hosted service can provide HTTPS. Provisioning either route is the controller's
job. HTTP Host validation at the destination still applies: a TCP forward does
not rewrite Host. Optional `httpHost` carries the authority's actual loopback
address when its port differs from the tunnel port. It may only override the port
on the endpoint's literal loopback IP, and only for HTTP. HTTPS routing overrides
and arbitrary Host names are refused. Omit it when no rewrite is needed.

The bridge rereads the file before every HTTP request, including cancellation.
Only the credential, expiry, and loopback HTTP authority port may change while that bridge lives. Changes to
the endpoint, scope, agent, or execution require a new bridge. Missing, expired,
malformed, shared, or symlinked files refuse requests without falling back to an
old token. Existing in-flight calls remain governed by the server's revocation
and receipt rules; a file update does not replay a mutation. Transient failures
are surfaced to the harness and are not silently retried by this bridge. After a
transport failure, the next tool call establishes a fresh upstream session using
the current file; the failed call itself is never replayed.

Use this mode without `OCTOBER_BUS_ADDRESS`, `OCTOBER_BUS_AGENT_TOKEN`, or local
registration flags. Do not put scope/admin credentials in the file or the agent's
environment. Unix private files are not a boundary against the same OS user.
Windows file ACL qualification is not implemented for this mode; existing local
discovery and managed-environment modes are unchanged.

The scope/agent/execution fields fence local renewal; they do not confer identity.
The receiving authority must authenticate the token and scope every tool call.
The controller must verify the physical execution before renewing access, revoke
its grant when that execution ends, and never put a replacement execution's token
under the old execution labels. Public Bus execution registration semantics are
unchanged: a newly registered execution requires a new bridge. This does not add
a same-execution token-rotation API to the standalone daemon.

Desktop Core accepts a standard execution Bearer on `/mcp` and derives the canvas
and node from its private registry. Conflicting legacy routing headers refuse.
Core administrator/forwarding credentials cannot impersonate an execution. This
uses the existing Desktop ledger and tools; the standalone Bus `/v1` service is
not installed as a second authority. Local manual-shell admission and physical
readiness checks still apply.

This connection support alone is not a complete remote-host adapter. SSH reach
supervision, host process evidence, hooks, safe input delivery, reconnect ownership,
installer qualification, and the hosted cloud controller remain integration work.
No package is published by these changes.
