# Changelog

All notable changes to Tether are recorded here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html). Pre-1.0: minor bumps for additive surface, patch bumps for fixes — breaking changes can land in any minor.

This file was backfilled from the git history and is a good-faith summary, not an exhaustive one; `git log` is the complete record. Entries are grouped by tagged release. Consumers should watch this file for new HTTP routes, MCP tools, CLI commands and catalog/configuration changes.

## [Unreleased]

Changes on `main` since v0.6.0.

### Added

- **An MCP server catalog entry can take a credential from a private file.** (CW-20261001-0229) `file:///abs/path` or `file://~/path` in `args:`, `env:`, `token:` or `url:` is read at spawn time, so the catalog YAML no longer has to carry the secret. The file must be a regular file owned by the current user with mode 0600 or tighter (a group- or world-readable file is refused), and a symlink must stay inside the catalog directory or the operator's home. A missing, empty or oversized file fails the load with an error that names the file and never contains its contents, and the value is redacted from an upstream's stderr tail, tool-call errors and launch records like a keychain value. Put the reference in `env:` to keep it off the command line; in `args:` the value is still on the upstream's `/proc/<pid>/cmdline`, and `tesseract mcp` and `hadrond mcp` currently take their token only as `--token`. A literal MCP-entry value that begins with `file://` is now read as a credential file. See `docs/secrets.md`.
- **Opt-in retention for the `events` table.** `daemon.events_retention` in `global.yaml` (`enabled`, default `false`; `days`, default 90, where 0 or negative turns it off) runs an hourly sweep. The sweep deletes events older than the window, by age only, in bounded batches, and logs how many it removed. Off by default, so upgrading deletes nothing; replay within the window is unchanged. The daemon's wake sweep and this sweep now share one periodic-job loop. (CW-20260930-0008)
- **Idempotent session create and resume.** `POST /sessions` and `POST /logical-agents/{id}/resume` accept an optional `idempotency_key`: a retry with the same key and request returns the session the first one created (`replayed: true`, HTTP 200), and the same key with a different request answers 409 `idempotency_conflict`. Launching a keyed session again is idempotent too. Keys are global and unauthenticated, so prefix them. Surfaced in the Go client (`ResumeLogicalAgentWithOptions`), the MCP `mux_session_create` / `mux_logical_agent_resume` tools, and `mux launch --idempotency-key` (CW-20260930-0229).
- **Messaging surfaces.** Scoped role/slot binding across CLI, MCP, HTTP and the Go client; a durable participant registry with canonical sessions and leased runtime bindings; durable hosted-session handoff with route fencing; delivery trace, operator repair and privacy-safe retention; and a bounded, server-only inbound A2A relay adapter.
- **MCP adapter: streamable-HTTP transport**, so an upstream restart no longer severs connected agents. Upstream `sse`/`http` connections and supervision now use the shared `go-mcp` client and supervisor packages.
- **Registry: derived/authored field split on project entries**, callback-backed derivation, an onboarding/lookup surface, flat authored `props`, and minter/owner provenance with symmetric redaction on write routes.
- **Launch profiles.** `config.Agent` is now `LaunchProfile`, with compositional launch resolution and a Global > Project > User settings cascade for onboarding configuration.
- **Runtimes.** An `opencode` runtime on the structured go-providers adapter (with resume verification and a skills compiler) and an Antigravity (`agy`) runtime with detect/seed/doctor support and provider events.
- **MCP proxy:** typed identifier extraction from proxied calls into session refs, and bounded workstream provenance on forwarded calls.

### Changed

- **Permission modes reach each provider as a posture, with no change to what an agent may do.** go-agent-wrapper v0.23.0 (agentkit v0.19.1, go-providers v0.39.0, go-sandbox v0.5.1) takes a launch's permission as a go-permission posture and refuses a provider's own spelling. Tether now maps `permission_mode` onto one posture per provider, the same in both launch engines:
  - claude: `bypass` runs with `--permission-mode bypassPermissions` instead of `--dangerously-skip-permissions`, and `default` runs with `--permission-mode default`.
  - codex: runs `workspace-write` with `approval_policy` `on-request` in both modes, as before. That is the policy under which its MCP tool calls are asked for and approved, rather than refused.
  - opencode: keeps its own defaults.
  - antigravity: `bypass` passes `--dangerously-skip-permissions`, and `default` passes `--mode accept-edits`.

  (CW-20261001-0156)
- **Client-visible: ACP launches (Copilot, Pi) are on.** The `TETHER_ENABLE_ACP` gate and its 403 `forbidden` answer are gone. The gate guarded against a go-agent-wrapper panic that took the daemon down when an ACP agent exited during launch, which v0.21.1 fixed. A launch whose agent exits early now fails like any other launch. The ACP runtime's `[permission_denied:…]`, `[auth_failed]` and `[session_lost]` log markers take the format the native runtimes write. (CW-20261001-0156)
- **Client-visible:** `POST /sessions/{id}/input` and `/turn` answer a lost provider resume id with the new `provider_session_lost` error code instead of `conflict`. The HTTP status stays 409, so status-only clients are unaffected, and the MCP `mux_session_send_input`/`mux_session_send_turn` tools report the same code. Callers that branched on `conflict` for this case should match the new code.
- Dependency refreshes: `agentkit` v0.8.0, `go-providers` v0.29.0, `go-messaging` v0.5.2.
- Planning history and design-dead docs were archived out of the repository tree.

### Fixed

- **Security: an agent naming a sandbox profile the catalog doesn't define is refused at launch, and no longer stops the daemon.** Before, such an agent failed catalog validation, so the daemon refused to start at all. Separately, `LaunchSession`'s own lookup silently skipped an unknown `permissions.default_sandbox`, so a catalog that reached it unvalidated ran the session with no sandbox and no warning. Now the launch path fails closed on its own: creating, launching or resuming a session of that agent fails with **404 `not_found`** naming the agent and the missing profile, and an `agent_file`/`agent_inline` override that names one is refused at create. Startup no longer fails for one such agent. The daemon logs a warning, `ValidateLaunch` refuses that agent's launches, and `mux doctor` has a new `catalog-sandbox-profiles` check. An empty `default_sandbox` still means no sandbox. (CW-20261001-0130)
- **Security: an `agent_file`/`agent_inline` override's sandbox profile is applied at launch, and may not loosen a pinned one.** Session create merged an override's `permissions.default_sandbox` into the effective agent, but `LaunchSession` read the base catalog agent's profile. So an override asking for a stricter sandbox was silently dropped, and the session ran with the catalog agent's profile, or none. The plan now records an override's profile (`sandbox_profile`, only when it differs from the catalog agent's). `LaunchSession` applies it, or refuses if the catalog doesn't define it (404). **Interim rule until per-caller identity (CW-20260930-0253):** an override may name any profile for a catalog agent that has none, but only the same profile for a pinned agent. Anything else is refused with **403 `forbidden`**, because an agent holding `session.write` could otherwise loosen its own sandbox through a child session. Sessions without an override, and plans created before this field, keep using the catalog agent's profile. (CW-20261001-0145)
- **The MCP proxy no longer drops a strict stateless http upstream whose ping it can't satisfy.** go-sdk v1.8.0's client sends ping without the per-request `_meta` that a stateless go-sdk server on protocol 2026-07-28 requires, so the server answers -32602. The proxy treated that as unreachable and dropped the upstream's tools; tangent hit this until it pinned its protocol versions. go-mcp v0.14.1 counts a JSON-RPC error reply to ping as reachable, in the handshake and the health probe. Transport failures and 429/502/503/504 still fail. Bumps go-mcp v0.7.0 → v0.14.1, with no other behavior change for Tether. (CW-20261001-0078)
- **An actor is no longer left bound to a session that has ended.** Two sessions for one logical agent hold stacked binding generations. Stopping the newer one revoked its binding and promoted the older generation, even when that session had already ended, so mail to the agent was stored and never woken. A daemon restart did the same for every session its sweep marked `failed`. Now a session's bindings are revoked when it ends (stop, natural exit or crash). Daemon startup revokes the bindings of every ended Tether session, which also clears bindings orphaned before this release; bridge bindings are untouched. A superseded generation whose session is still running still becomes current again. **Client-visible:** `POST /messages/notify` to an agent whose binding names a session that is not running now answers `wake_reason: "session-not-running"` instead of a bare `wake_attempted: false`. (CW-20260912-0134)
- **A wake whose turn was submitted no longer lapses into retry before the agent reads its message.** `POST /messages/notify` (and the wake sweep) acked a wake to `turn_submitted` and left its 30-second lease for the recipient's consumption to close. An agent's turn takes longer than that, so every successful wake went to `retry_scheduled` with "lease expired", even after the agent had read and answered the message. The agent-os smoke run hit this with a message pulled through `mux_message_inbox`, which never settled a delivery. A submitted wake now holds its lease, still at `turn_submitted` (submission is not consumption), for up to 15 minutes. The recipient's consumption closes it as `delivered`. A wake that is never consumed lapses into one retry, which wakes the recipient again as a reminder. `GET /messages/inbox` takes `as_session`: when that live session is the recipient itself, the pulled messages are consumed, and `mux_message_inbox` passes its own session. An operator listing someone else's inbox still settles nothing. A retry that finds the message already consumed or read settles it without waking again; this covers a consume during a busy/offline backoff, when the delivery core refuses the claim. Busy, offline and failed-submit wakes still retry as before. (CW-20261001-0016)
- **Client-visible: a stopped session ends in `killed`, not `completed`.** `POST /sessions/{id}/stop` (and `mux sessions stop`, the MCP `mux_session_stop` tool and ACP session close, which all route through it) recorded the session as `completed`, with whatever exit code the process returned on the way down: `-1` when a signal ended it, and `0` when it exited cleanly on `SIGTERM` (the agent-os smoke run saw `completed`/`0` for a session hung on stdin). A host could not tell a cancel from an agent that finished without a result. The session row, `GET /sessions/{id}`, `mux sessions get` and the terminal `session.state_changed` event now say `killed`, the terminal state the API already documented but never wrote. The exit code is unchanged and still the process's own, so branch on `state`. Sessions that end on their own stay `completed` (exit 0) or `failed`, and the daemon-restart sweep still records `failed` with exit `-1`. Clients that treated `completed` as "ended" should add `killed`; `state` is a plain string in the Go client, so no client release is needed. (CW-20260930-0250)
- **A2A relay tasks survive a daemon restart, and a task a restart cut off is failed rather than left 'working'.** The inbound A2A relay used the A2A SDK's in-memory task store, so every delegated task an external peer had been told about disappeared on restart (`tasks/get` → not found). Task records now persist per binding in the new `a2a_tasks` table (migration 0032), so `tasks/get` answers after a restart and one binding still cannot read another's tasks. The wait a delegated task blocks on is process-local and cannot be persisted, so on startup any task left in `submitted` or `working` is moved to `failed` with a message saying the restart interrupted it; the relayed message itself is unaffected and the peer resubmits. Settled tasks, including `input-required` ones, are left as they were. (CW-20260930-0063)
- **`mux daemon stop` no longer signals a process it has not identified as muxd.** The pidfile is removed only on graceful shutdown, so after a crash it can name a PID the OS has since handed to an unrelated process, and `stop` sent `SIGTERM` to whatever held the number. `stop` now reads the PID's command line (via `go-localdaemon`'s `VerifyCommand`) and signals only a `mux daemon run` process; a live PID that is not muxd is reported and the stale pidfile removed, and a check that cannot be made refuses to signal. The start guards (`daemon start`, `daemon run`, the server's own pre-flight and `WritePIDFile`) and `daemon status` apply the same identity check, so a recycled PID no longer makes start refuse or status report a live daemon; when identity cannot be checked they still err toward "already running". Adds a dependency on `go-localdaemon` v0.1.0. (CW-20260930-0045)
- Catalog reload failures are redacted in MCP output, and launch catalog reads refresh correctly.
- Six registry re-onboarding and group-props bugs found in review.
- The messaging pull-only fencing gap, a binding-lease TOCTOU race, and a late-Nack-reverses-Consume race.

## [0.6.0] - 2026-09-11

### Added

- Group messaging, the federation directory (registry) and authority-routing peer configuration for messaging.
- An AI gateway runtime with streaming chat events over MCP, Gemini/embeddings/multimodal shorthands, an Ollama-compatible fallback, and OpenAI key setup through the keychain.
- OpenTelemetry propagation and AI spans; adoption of `agentkit` v0.2.0 for the runtime.
- Curated MCP proxy discovery, a notify wake path for messages, and Task-bundle planting for Codex worktrees.
- Sysop UI served at the site root with Operations as the home view.

### Changed

- Storage paths fall back to `go-apppaths`; release-readiness hardening of install and Sysop UI build paths.

## [0.3.0] - 2026-05-14

### Added

- Three-layer catalog discovery with schema extensions, a skills package with Claude and Codex compilers, and `mux agents create|edit|show`.
- A caller-provided launch surface and session-mutating MCP tools routed through the daemon.
- An ACP server (`mux acp`) bridging to the session service.

### Fixed

- Boot-command namespace collision; `acpsvc` infinite loop on attach error.

## [0.2.1] - 2026-05-09

### Fixed

- Patch release over v0.2.0; see `git log v0.2.0..v0.2.1` for detail.

## [0.2.0] - 2026-04-27

### Added

- Codex CLI support (boot, launch); ephemeral temp-dir model for Claude sessions; selective flat MCP proxy with a `--servers` filter (`--broker` deprecated).
- Message routing contract and durable messaging store with `/messages/*` HTTP routes; event durability and an observation query surface; a provider compliance suite and capability matrix.
- Sandbox profile registry integrated with the catalog; checkpoint/resume with logical-agent resume.
- `mux_discover` returns `totalMatches` so callers can detect truncation.

### Fixed

- MCP event forwarder and proxy-polling bugs; `mux_message_inbox` kind filter description aligned with the valid enum.

## [0.1.0] - 2026-04-21

### Added

- Initial release: the `muxd` daemon with session lifecycle, PTY/process management, an attach broker with ring-buffer fan-out and `since_seq` resume, session lifecycle events, an API/stub runtime, launch resolution with boot-prompt composition, the `mux` CLI, and golangci-lint-gated quality checks.

[Unreleased]: https://github.com/hollis-labs/tether/compare/v0.6.0...HEAD
[0.6.0]: https://github.com/hollis-labs/tether/compare/v0.3.0...v0.6.0
[0.3.0]: https://github.com/hollis-labs/tether/compare/v0.2.1...v0.3.0
[0.2.1]: https://github.com/hollis-labs/tether/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/hollis-labs/tether/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/hollis-labs/tether/releases/tag/v0.1.0
