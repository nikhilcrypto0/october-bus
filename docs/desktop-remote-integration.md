# October Desktop remote integration contract

Status: Bus-side implementation complete for the Agents Anywhere release; Desktop
follow-ups listed at the end. Branch `feat/desktop-remote-agents` (worktree
`/private/tmp/october-bus-agents-anywhere`), based on `20b7456` and including
the hosted gateway commit `dba3ece` (PR #120) by merge.

This document is the controller↔helper contract. The connection file itself is
specified in [managed-connections.md](managed-connections.md); this page covers
ownership, commands, artifacts, evidence and what remains on the Desktop side.

## Ownership

| Component | Owns |
| --- | --- |
| October Bus native helper (`october-bus`) | MCP bridge over a controller-managed connection, per-request credential reread, next-call reconnect without replaying a failed mutation, Claude/Codex lifecycle hook, bounded setup check, hosted-gateway self-registration. |
| October Desktop / Core | Canvas identity, execution credentials and their renewal/retirement, the connection file writer, SSH supervision and the loopback route, tmux ownership and foreground proof, safe input delivery, helper installation, Settings cards, readiness decisions. |
| October Cloud controller | Hosted authority, outbound route, worker admission and lifecycle while the laptop is off. Not part of this handoff. |

There is one authority per collaboration scope. The helper never starts a second
daemon or database for a Desktop workspace; it forwards Desktop Core's actual
MCP tools and reports lifecycle to Core's existing `/hook/*` routes.

## Commands

All three read the same connection file, from `--connection-file` or
`OCTOBER_BUS_CONNECTION_FILE`. Stdout is reserved for the requested protocol.

| Command | Role | Exit |
| --- | --- | --- |
| `october-bus mcp stdio --connection-file <path>` | Long-lived MCP bridge for the harness. Refuses if launcher credentials, local registration flags or `--remote` are also present. | Non-zero only on startup failure; runtime failures surface as tool errors plus one stderr line per failure class. |
| `october-bus hook [--connection-file <path>] <event> [claude\|codex]` | Per-event lifecycle reporter, invoked by the harness. Same argv shape as Desktop's `bus-hook.mjs` (`<event> [<flavor>]`). | Always 0. Silent and offline when no file is named. |
| `october-bus mcp check [--connection-file <path>] [--json]` | Setup diagnostic: file validation plus one JSON-RPC `ping`. Never `initialize`, never touches the inbox. | 0 on `ok`, 1 otherwise. |
| `october-bus version [--json]` | Version pin verification: `{"runtimeVersion","protocolVersion"}`. | 0 |

Failure classes for all of them: `invalid-configuration`, `expired-credential`,
`retired-execution`, `unreachable`, `refused-credential`, `authority-unavailable`,
`session-lost`, `protocol`. Definitions are in
[managed-connections.md](managed-connections.md#diagnostics).

## Connection record: file ownership, renewal, retirement

- **One record per run.** Path is chosen by the controller, for example
  `~/.october/executions/<execution>/connection.json`, directory `0700`, file
  `0600`, absolute, no symlinks. Windows is not supported for this mode.
- **Labels.** `scopeId` = canvas id, `agentId` = node id, `executionId` = the
  terminal epoch Core knows as `launch`. They pin the bridge and are sent as
  `canvas`/`node`/`launch` on hook routes. They are never authority; Core
  resolves identity from the Bearer and refuses conflicting labels.
- **Credentials.** `agentToken` is the execution's MCP capability (the token
  Desktop registers with `capability.bindMcp`, `terminal:<epoch>`). `hookToken`
  is the execution's hook capability (the token `bindHookCapability` mints for
  the same `terminal:<epoch>` binding). Both are 32 random bytes, unpadded
  base64url. Nothing else ever enters the file: not the Core desktop/CLI
  credential, not the process-lifetime `busHookToken()`, not a scope token.
- **Renewal.** Write a new file and `rename(2)` it over the old one. The bridge
  rereads before every request, so a renewal takes effect on the next call
  without restarting the harness. `expiresAt` is the controller's deadline; an
  expired file refuses every request until renewed.
- **Retirement.** Delete the file, or change any label. Either withdraws the
  bridge: subsequent calls fail `retired-execution` and never fall back to an
  old token. A replacement execution gets a new file, a new bridge and new
  credentials; its credentials must never appear under the old labels.
- **Route.** `endpoint` is HTTPS or literal-loopback HTTP; `httpHost` carries
  Core's real port when the SSH reverse-forward port differs. Redirects refuse.
  The hosted gateway route uses HTTPS with `/bus/mcp` and no `httpHost`; its
  `/hook/*` routes are not served, so lifecycle hooks are Desktop-authority only.

## Behavior guarantees the helper provides

Verified by the tests listed under evidence:

- A failed call is never replayed. Losing the response of an accepted
  `message_peer` yields a tool error; the next call reconnects and the message
  is delivered exactly once.
- Cancelling a long human wait (`ask_user`-style tool) keeps the upstream
  session; concurrent calls during the wait succeed on the same session; no
  reconnect follows.
- A refused or expired credential fails the call, prints one stderr line, and
  the next call after renewal reconnects and succeeds.
- An authority restart (HTTP 404 on the old session) and a JSON-RPC error on
  HTTP 400 both cause exactly one reconnect on the next call; the worker is never
  stranded on a dead SDK session.
- A file whose execution labels changed, or whose file was withdrawn, refuses
  without contacting the authority.
- `mcp check` sends no `initialize` and no tool call, so it cannot mark the
  agent ready or consume an inbox message.
- Hooks acknowledge a pulled receipt only after stdout accepted the bytes; a
  subagent/sidechain stop is ignored; malformed stdin behaves like `{}`; a hook
  outside a managed execution makes no network request.

## Harness configuration on the worker

See [examples/desktop-remote](../examples/desktop-remote/) for complete files.

- **Claude Code.** Per-launch `settings.json` hooks call
  `"<helper>" hook <event>` with the same events and args as
  `claudeLaunchSettings` today (`session-start`, `pre-prompt`, `stop`,
  `stop-failure`, `session-end`, `notify`, `permission-request`,
  `post-tool-use`, `post-tool-use-failure`, `ask-user-question`,
  `ask-user-question-resolved`). The per-launch `.mcp.json` runs
  `"<helper>" mcp stdio --connection-file <path>`.
- **Codex.** `hooks.json` calls `"<helper>" hook <event> codex` for
  `SessionStart`, `UserPromptSubmit`, `Stop`, `PermissionRequest` (args
  `session-start codex`, `pre-prompt codex`, `stop codex`, `notify codex`).
  `config.toml` runs `"<helper>" mcp stdio` with no path; the controller exports
  `OCTOBER_BUS_CONNECTION_FILE` into the tmux session environment. A Codex run
  outside October on that machine then fails MCP startup for that server entry
  rather than receiving an empty tool list; that is the public Bus rule.
- Hook stdout contracts are unchanged from the Node hook: Claude raw text on
  `pre-prompt`, `SessionStart` envelope on `session-start`; Codex
  `UserPromptSubmit` envelope.

## Artifacts, build, install and verification

Supported managed-worker architectures: Linux x86-64 and Linux arm64 (static,
`CGO_ENABLED=0`, no libc, no Node). macOS and Windows archives exist for other
uses; the connection-file mode is Unix-only.

Release artifacts (`.github/workflows/release.yml`, tag `v<version>`):

| Artifact | Contents |
| --- | --- |
| `october-bus_<version>_linux_amd64.tar.gz`, `october-bus_<version>_linux_arm64.tar.gz` | Directory `october-bus_<version>_linux_<arch>/` containing `october-bus`, `october-bus-conformance`, `LICENSE`, `README.md`, `docs/`, `spec/`, `adapters/`, `compatibility/`, `assets/`. |
| `checksums.txt` | `sha256sum` lines for every release file; verify with `sha256sum -c`. |
| `october-bus_v<version>_linux_<arch>.spdx.json` | SPDX SBOM. |
| GitHub build-provenance attestation | Verify with `gh attestation verify <archive> --repo october-dev/october-bus`. |

The release workflow smoke-tests (`version`, `demo`) the Linux amd64 archive on
`ubuntu-latest` and, added in this branch, the Linux arm64 archive on
`ubuntu-24.04-arm`, before publishing.

Install on a worker (the controller performs this over SSH; users do nothing):

```sh
VERSION=<pinned version>; ARCH=<amd64|arm64>
ARCHIVE="october-bus_${VERSION}_linux_${ARCH}.tar.gz"
# Download the archive and checksums.txt on the laptop from the GitHub release,
# verify, then stream the archive over SSH stdin as the existing installer does.
sha256sum --ignore-missing -c checksums.txt        # must print: ${ARCHIVE}: OK
gh attestation verify "${ARCHIVE}" --repo october-dev/october-bus
tar -xzf "${ARCHIVE}" "october-bus_${VERSION}_linux_${ARCH}/october-bus"
install -m 0755 "october-bus_${VERSION}_linux_${ARCH}/october-bus" \
  ~/.october/servers/<install>/versions/<digest>/bin/october-bus
~/.october/servers/<install>/versions/<digest>/bin/october-bus version --json
# {"protocolVersion":"0.1","runtimeVersion":"<pinned version>"}
```

Pin the release checksum of the archive (from `checksums.txt`) in the same
committed catalog Desktop already uses for the Node runtime, and compare
`runtimeVersion` after installation. No daemon is started; no credential is
copied by the installer.

Build the same artifact layout locally from a checkout (for pre-release
integration; not a release):

```sh
VERSION=$(node -p "require('./sdk/typescript/package.json').version")
for ARCH in amd64 arm64; do
  DIR="october-bus_${VERSION}_linux_${ARCH}"; mkdir -p "dist/${DIR}"
  CGO_ENABLED=0 GOOS=linux GOARCH=${ARCH} go build -trimpath \
    -ldflags "-s -w -X github.com/october-dev/october-bus/bus.Version=${VERSION}" \
    -o "dist/${DIR}/october-bus" ./cmd/october-bus
  tar -C dist -czf "dist/${DIR}.tar.gz" "${DIR}"
done
(cd dist && sha256sum *.tar.gz > checksums.txt)
```

The optional npm route (`@october-dev/october-bus@0.1.0-next.14` with
`@october-dev/october-bus-linux-{x64,arm64}` optional dependencies, launcher
`cli/october-bus.cjs`) carries the same binary but requires Node 20+ on the
worker; it is documented in [releases.md](releases.md) and is not the managed
install path. Nothing was published by this work.

## Evidence (2026-09-21, macOS arm64 host, Go 1.27.0, go-sdk v1.7.0)

Synthetic executions and local fixtures only. No live SSH host, no live
Claude/Codex model, no cloud provisioning, no publication.

| Check | Result |
| --- | --- |
| `go test ./cmd/october-bus ./internal/... -count=1 -race` | ok (20.4 s, 2.5 s) |
| `go test ./... -count=1` | ok, all five packages |
| `go vet ./cmd/october-bus ./internal/...`, `go build ./...`, `gofmt -l` | clean |
| Desktop opt-in `OCTOBER_BUS_TEST_BINARY=… vitest run src/core/agent-api.integration.test.ts -t 'native Bus bridge'` | 1 passed, 36 skipped. Real helper ↔ temporary Core through a TCP forward: send, reply, inbox pull, receipt ack, renewed credential, refusal recovery, execution-change refusal. |
| Linux cross-builds | `ELF 64-bit LSB executable, x86-64 … statically linked` and `… ARM aarch64 … statically linked`; archives 6.2 MB and 5.7 MB. |
| Fixture exercise (`examples/desktop-remote`) | `mcp check` → `unreachable`, exit 1; bound hook → one stderr line, exit 0; unbound hook → silent, exit 0; `mcp stdio` without identity → refuses. |

New Go tests: `TestManagedMCPBridgeCancellationAndConcurrencyKeepSession`,
`…RecoversAfterRefusalWithoutReplay`, `…ReconnectsAfterAuthorityRestart`,
`…SurvivesProtocolRefusalOnHTTPError`, `…RenewsExpiredCredentialWithoutRestart`,
`…ReadsConnectionPathFromEnvironment`, `TestHook*` (seven), `TestMCPCheck*`
(two), `TestVersionJSON`, alongside the preserved checkpoint tests for lost
responses and private-file validation.

Defects fixed in the preserved candidate: reconnect policy keyed on the Go
error type kept a session the SDK had already failed whenever an HTTP 4xx body
was a JSON-RPC error (worker stranded); the checkpoint tests raced on a shared
stderr buffer under `-race`.

Not verified here: Linux arm64 execution (cross-built only; CI smoke added but
not yet run on a tag), real Claude/Codex hook payloads on a remote host, the
hosted gateway with a live TLS endpoint, laptop-off operation.

## Limitations

- Connection-file mode is Unix-only; Windows ACL checks are not implemented.
- Hooks support `claude` and `codex` only; other flavors are refused on stderr.
- Hooks are Desktop-Core routes on the endpoint's origin; the hosted gateway
  does not serve them.
- `mcp check` proves route and credential admission, not agent readiness. Only
  the controller's process/foreground evidence can say the model is ready or
  that a shell is safe for injected text.
- A helper older than this branch refuses a connection file containing
  `hookToken`; Desktop pins the helper, so no compatibility shim is provided.
- A transport failure costs the failing call; the following call reconnects.
  The helper cannot know which tools are idempotent and does not retry.

## Commits

- `6a69cea` preserved Desktop-agent checkpoint (`--connection-file`, reconnect).
- `bdb26a6` outcome-aware reconnect, failure classes and diagnostics, `hook`,
  `mcp check`, `version --json`, `OCTOBER_BUS_CONNECTION_FILE`, tests.
- `c6f66cc` merge of `feat/hosted-mcp-gateway` (`dba3ece`, PR #120).
- The documentation, fixture and release-workflow commit that follows.

The source checkout's uncommitted `README.md` edit belongs to another session
and was not touched.

## Required Desktop follow-ups

1. **Connection file writer.** Add `hookToken` (the `terminal:<epoch>` hook
   capability from `bindHookCapability`) next to `agentToken`; renew both by
   atomic rename; delete the file on retirement; never reuse labels for a
   replacement execution. Keep `scopeId`/`agentId`/`executionId` =
   canvas/node/epoch.
2. **Environment.** Export `OCTOBER_BUS_CONNECTION_FILE=<path>` into the tmux
   session environment for every managed launch (Codex needs it; Claude may use
   the explicit flag in its per-launch `.mcp.json`).
3. **Hook installation.** Generate hook commands as `"<helper>" hook <arg>` with
   the existing `arg` strings instead of `"<node>" "<script>" <arg>`; extend
   `isOctoberBusHookCommand`/migration recognition and the Codex trust-hash
   seeding to the helper form. Existing routes, bodies and envelopes are
   unchanged, so Core needs no change for hooks.
4. **Installer.** Install the pinned Linux `october-bus` from the release
   archive (checksum + attestation verified on the laptop) under the existing
   `~/.october/servers/<install>/versions/<digest>/` layout; verify
   `version --json`; show installation, `mcp check` class and agent readiness
   as three separate facts in the Settings cards.
5. **Delivery.** Wake/inject remains `tmux send-keys` under Desktop's existing
   foreground proofs; the hook's `session`/`notify`/`stop` reports feed those
   decisions. `/hook/wake` is not used by Claude/Codex.
6. **Cross-repository test.** Keep `OCTOBER_BUS_TEST_BINARY` pointing at a build
   of this branch; the `native Bus bridge` test contract is unchanged.
