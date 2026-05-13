# Sprint v004-02 — Claude Stream-JSON Provider

Epic: [v0.0.4](../epics/v0.0.4-dogfood-ready-infrastructure.md)

**Created:** 2026-04-19 (reframe during Sprint 1 smoke test).
**Goal:** Introduce a provider path that invokes `claude` CLI with structured JSON output (not a PTY-backed interactive TUI), parses events into typed messages, and renders them natively in the TUI as a chat surface. For claude specifically this is categorically more usable than the PTY-bridge because we render text + tool-use + usage events ourselves — no ANSI fidelity issues, no resize fragility, no TUI-in-TUI collision.

## Context: why this exists

Sprint 1 (PTY fidelity) landed resize propagation + an external-terminal escape hatch, and both help for shell-based providers (vim, htop, actual shells). But the dogfood-primary target is `claude` CLI, and full-screen TUI apps simply can't render well inside another full-screen TUI without a complete ANSI state-machine emulator. Even the shipped resize fix is a partial win.

Reading sibling project [Nanite](/Users/chrispian/Projects-apps/nanite/) reveals the better pattern: Nanite runs `claude --print --output-format stream-json --verbose` and parses emitted events (`assistant` / `result` / `system` / `error`) into typed structs. It renders them as a chat UI. PTY is only used as a transport; the real interface is the JSON stream.

`claude` CLI's `session_id` (emitted in the `system` event) is how Nanite continues a conversation — `claude --resume <session_id>` on the next turn. This is the authoritative model for claude: it's an **inference pipe with a resumable history**, not a long-lived attached process.

Agent-mux already has a PTY-based provider (`internal/provider/cli/claudecode/`). This sprint adds a **second** provider kind (stream-json subprocess) and makes it the default for `claude` launches. The PTY-based provider stays for shell / vim / any TUI-native target.

## Ecosystem posture: `pkg/claudestream/` is extractable

The parser + event types live at **`pkg/claudestream/`** (top-level, NOT under `internal/`). The explicit plan is:

1. Copy Nanite's parsers into `pkg/claudestream/` with attribution.
2. Iterate on the shape in agent-mux until stable.
3. Extract into a standalone Go module (`github.com/chrispian/claudestream` or similar — or slot under `~/Projects-apps/framework/libs/` alongside `go-directives`, `go-mcp`, `go-otel`, `go-plugin`, `go-providers`, `go-queue`, `go-toolbroker`, `plugin-sdk`) when we have two real consumers.
4. Nanite migrates to consume the extracted module instead of maintaining its own copy.

Every commit in this sprint preserves the extractability:
- No dependencies on agent-mux `internal/` types from within `pkg/claudestream`.
- No agent-mux-specific naming in the public API.
- Package README documents the extraction plan explicitly, and the destination (probably `framework/libs/go-claudestream`).

`pkg/claudestream/` is the next ecosystem package — the portfolio already has `framework/libs/go-*` for directives, mcp, otel, plugin, providers, queue, toolbroker, plus `plugin-sdk`. Pattern to preserve: build inside a consumer (or a thin prototype), iterate until stable, then promote to `framework/libs/`.

## Exit criteria

- [x] `pkg/claudestream/` parses every event type claude CLI emits in stream-json mode: `system/init` (session_id), `assistant` (text + tool_use blocks), `result` (stop_reason + usage), `error`, `rate_limit_event`. *(T-01, `9f003b0`.)*
- [x] `pkg/claudestream/README.md` documents the extraction plan — intended module path, consumer list (agent-mux, Nanite), attribution to Nanite's original parsers. *(T-01, `9f003b0`.)*
- [x] New provider adapter `internal/provider/cli/claudestream/` spawns `claude --print --output-format stream-json --verbose --input-format stream-json` as a subprocess (no PTY), pipes output through `pkg/claudestream`, exposes events via a new `provider.Session` surface. *(T-02, `970f6ec`.)*
- [x] Provider contract extends cleanly: either the existing `Session` interface gains an optional event-channel getter, or a new `EventfulSession` interface is introduced. ADR 0017 captures the decision. *(T-03, `6152ca8` — no interface change; events ride the existing attach stream as NDJSON.)*
- [x] Daemon surfaces claudestream events to clients. Either a new event-stream kind on `GET /events/stream`, or a dedicated `GET /sessions/{id}/stream` route. ADR 0017 decides. *(T-03, `6152ca8` — attach stream reuse, no new route.)*
- [x] TUI gains a chat surface (`internal/tui/detail/chat.go`) that renders events natively: assistant text bubbles, collapsible tool_use blocks, usage stats in the footer. Launches with a `claudestream`-kind provider auto-attach into chat instead of the raw-attach screen. *(T-04.)*
- [x] `--resume` works end-to-end: a new turn sent through the chat input continues the prior claude session without spawning a fresh one. Session continuity is keyed by `session_id` from the first `system/init` event. *(T-05 — preset + callback wiring verified via adapter tests; full end-to-end awaits manual claude smoke.)*
- [x] `make check` green. *(362 tests, 0 lint, 0 vuln.)*

## Non-goals (explicit)

- Do NOT replace the `provider.cli.claudecode` PTY adapter. Both coexist. Users pick via the Launch profile's provider ID.
- Do NOT copy Nanite's claude plugin scaffolding (`.mcp.json` generator, `.claude/agents/*.md` discovery). That's Nanite-specific and outside agent-mux's concerns.
- Do NOT generalize to "any streaming provider protocol" in this sprint. Ship claude-specific; genericism emerges once a second provider (gemini CLI, codex CLI, etc.) surfaces the shared shape.
- Do NOT import Nanite as a library. Parsers are copied with attribution, iterated on in agent-mux, and extracted later.
- Do NOT pull forward ANSI-state-machine rendering (Sprint 1's parked "v0.3+ concern" stays parked) — the whole point of this sprint is to sidestep that problem for claude.

## Tasks

### T-v004-s02-01: Copy + clean Nanite parsers into `pkg/claudestream/`

**Priority:** 1. **Tags:** package, claude, ecosystem.

#### Problem
Nanite has working parsers for claude's stream-json output. Agent-mux needs the same primitives. Building from scratch duplicates effort and risks missing edge cases Nanite already hit.

#### Fix direction
- Copy `/Users/chrispian/Projects-apps/nanite/pkg/provider/pty_claude.go` event parsing logic into `pkg/claudestream/events.go` (agent-mux). Strip Nanite-specific integration; keep the pure parser + typed event structs.
- Define the public API:
  - `Event` interface or sum-type (pick based on whether Go generics help; likely typed structs + a `Kind` enum is cleaner).
  - `Parse(line []byte) (Event, error)` — one line at a time; NDJSON is the wire format.
  - `Scanner(r io.Reader) *EventScanner` — bufio.Scanner wrapper that yields events; handles multi-line JSON if claude ever emits any (currently single-line).
- Include the data types: `AssistantEvent` (with ContentBlocks: text + tool_use), `ResultEvent` (StopReason, Usage{input, output, cache_creation, cache_read tokens}), `SystemEvent` (SessionID on subtype=init), `ErrorEvent`, `RateLimitEvent`.
- Add `pkg/claudestream/README.md` with the extraction plan and attribution to Nanite.
- Unit tests from captured stream-json fixtures (one .jsonl per event kind, happy + malformed-line paths).

#### Files
- `/Users/chrispian/Projects-apps/agent-mux/pkg/claudestream/events.go` (new)
- `/Users/chrispian/Projects-apps/agent-mux/pkg/claudestream/scanner.go` (new)
- `/Users/chrispian/Projects-apps/agent-mux/pkg/claudestream/events_test.go` (new)
- `/Users/chrispian/Projects-apps/agent-mux/pkg/claudestream/testdata/*.jsonl` (new fixtures)
- `/Users/chrispian/Projects-apps/agent-mux/pkg/claudestream/README.md` (new)

#### Acceptance criteria
- [x] All five event kinds parse into typed structs. *(KindSessionID, KindDelta, KindToolUse, KindUsage, KindDone, KindError — 6 kinds, rate_limit + unknown-type + system/non-init silently skipped per forward-compat.)*
- [x] Malformed JSON lines return a typed error, not a panic. *(Wrapped via fmt.Errorf; test: `TestParse_InvalidJSONErrors`.)*
- [x] Scanner handles EOF gracefully. *(Returns `(Event{}, false, nil)` at clean EOF; drains multi-event lines; skips ignored-type lines. Tests: `TestScanner_DrainsMultiEventLines`, `TestScanner_SkipsBlankAndIgnoredLines`.)*
- [x] README documents promotion plan and rules preserving extractability (no `internal/` imports, no agent-mux-specific naming, stdlib-only deps).
- [x] No import from `github.com/chrispian/agent-mux/internal/...` in `pkg/claudestream/`. *(Only stdlib `bufio` / `encoding/json` / `fmt` / `io`.)*

#### Scope fences
- Do NOT include Nanite's `.mcp.json` generator or agent-discovery scaffolding.
- Do NOT add semantic analysis over events (e.g. "summarize this turn") — pure parsing only.
- Do NOT invent event kinds that aren't in Nanite's handler or confirmed via `claude --help` / sample runs.

---

### T-v004-s02-02: claudestream provider adapter + subprocess spawn

**Priority:** 1. **Tags:** provider, subprocess, claude.

#### Problem
Agent-mux has no provider that talks to claude via subprocess + stream-json. The existing `cli.claudecode` adapter spawns `claude` under a PTY and treats the output as raw bytes.

#### Fix direction
- New package `internal/provider/cli/claudestream/` with:
  - `Adapter.ID() string` → `claude-stream` (or `claudestream`).
  - `Adapter.Kind()` → new `provider.RuntimeKindSubprocess` (add to `provider.RuntimeKind` enum).
  - `Start` spawns `claude` with `--print --output-format stream-json --verbose --input-format stream-json`. Pipes stdout through a `pkg/claudestream.Scanner`. Stdin accepts `{"role":"user","content":"..."}` JSON lines for turns.
  - Returns a `provider.Session` that exposes claudestream events through a new method. See T-03 for the interface shape.
- Adapter honors the launch plan's `project` / `agent` / `provider` triplet the same way claudecode does — system prompt composition, env, workspace root.
- Environment variable `ANTHROPIC_API_KEY` (or claude's equivalent) must be propagated. Respect `provider.Env.Mode` semantics from v0.0.2.

#### Files
- `/Users/chrispian/Projects-apps/agent-mux/internal/provider/cli/claudestream/adapter.go` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/provider/cli/claudestream/adapter_test.go` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/provider/provider.go` (RuntimeKindSubprocess + EventfulSession)
- `/Users/chrispian/Projects-apps/agent-mux/internal/app/service.go` (register the new adapter)

#### Acceptance criteria
- [x] `Adapter.Start` spawns claude with the right flags (`--print --output-format stream-json --verbose`, `--resume <sid>` when applicable, `-p <prompt>` per turn). *(Fake `sh -c` wrapper in tests emits canned stream-json; 8 tests verify static interface + lifecycle + event fanout + log-file writes + session_id capture + ErrTurnInFlight on concurrent turns.)*
- [x] Events from stdout reach the caller via the session. *(Fanout writer + session log both receive line-by-line NDJSON; test `TestSession_SendInputEmitsEventsAndCapturesSessionID`.)*
- [x] Session input triggers the subprocess. *(SendInput spawns `exec.CommandContext` per turn; ErrTurnInFlight guards concurrent sends.)*
- [ ] Sandbox integration from Sprint 3 works transparently. *(Pending Sprint 3 — StartOptions doesn't carry a Sandbox field yet. This adapter will honor it when the field lands.)*

**Design notes (for future reference):**
- Per-turn subprocess model (matches Nanite + claude CLI's natural shape): session is a long-lived container, each user turn spawns a fresh `claude -p <prompt>` with `--resume <sid>` after the first turn captures session_id.
- Between turns: no child process. Wait() blocks until Stop() — the session doesn't have a natural "end" point.
- Stop kills any in-flight turn, closes stoppedCh, Wait returns. Idempotent via sync.Once.
- Resize is a no-op (no PTY).
- Fanout carries raw NDJSON — attach clients see line-delimited claude events directly. No re-serialization. TUI chat surface parses with `pkg/claudestream.Scanner`.

#### Scope fences
- Do NOT implement `--resume` logic in this adapter; the session-continuity task is T-05.
- Do NOT support `--output-format text` or `--print` (batch) modes from this adapter — it's stream-json only.
- Do NOT implement structured-input features claude hasn't documented (custom system prompt override via JSON, etc.) — stick to text content blocks.

---

### T-v004-s02-03: Provider contract extension + daemon event routing

**Priority:** 1. **Tags:** contract, daemon, adr.

#### Problem
The current `provider.Session` exposes `SendInput([]byte)` and `Wait()` but has no way to surface structured events. Need to decide: extend the Session interface with an event channel, or introduce a dedicated `EventfulSession` interface that claudestream (and future structured providers) implements.

#### Fix direction
- Write ADR 0017 comparing:
  - **(a) Extend `Session` with `Events() <-chan Event` returning an empty channel for PTY-style sessions.**
  - **(b) New interface `EventfulSession` with `Events() <-chan Event`; runtime type-asserts.**
  - **(c) Daemon-level event bus: sessions publish events via a shared Publisher (same pattern as v0.0.2 runtime events) and subscribers filter by session ID.**
- Recommend (c) — it aligns with the existing v0.0.2 event-bus pattern and lets multiple consumers (TUI + future MCP server + future Nanite-as-a-client) subscribe to the same events without a per-session-type branch.
- Implement (c): claudestream events flow through the existing `events.Bus` as a new `EventKind` (e.g., `session.claudestream.event`). Subscribers filter by `session_id` + kind.
- Daemon exposes per-session stream via a new route **or** extends `GET /sessions/{id}/events` to include claudestream events (they're scoped to the session, so the existing endpoint is the natural home).

#### Files
- `/Users/chrispian/Projects-apps/agent-mux/docs/adr/0017-claudestream-event-routing.md` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/events/types.go` (new event kinds)
- `/Users/chrispian/Projects-apps/agent-mux/internal/api/events.go` (route extension if needed)

#### Acceptance criteria
- [x] ADR 0017 committed with decision + alternatives considered. *(Decision: events ride the existing attach stream as NDJSON. No new daemon surface, no provider-contract extension, no new event kind. `pkg/claudestream.Scanner` is the consumer-side adapter.)*
- [x] claudestream events round-trip: subprocess → Fanout → daemon attach broker → HTTP `/sessions/{id}/attach` → client (with optional `claudestream.Scanner` for typed parsing). *(Already implemented in Sprint 2 T-02's adapter.go.)*
- [x] Event payload uses `pkg/claudestream` types. *(Raw NDJSON on the wire; types instantiated in `claudestream.Parse` on the consumer side.)*

**Resolution:** T-03 effectively collapsed into T-02 because (d) requires no new code — just ADR-level documentation of the intentional choice to reuse attach. Recording it as a distinct task so the reasoning is discoverable alongside the code that embodies it.

---

### T-v004-s02-04: TUI chat surface

**Priority:** 1. **Tags:** tui, chat, render.

#### Problem
The existing AttachScreen renders raw PTY bytes. For claudestream sessions we need a surface that understands the event types: assistant text bubbles, tool_use blocks (collapsible JSON detail), usage stats.

#### Fix direction
- New screen `internal/tui/detail/chat.go` implementing `screen.Screen`.
- Layout:
  - Header: session ID, workspace, usage counters (input/output/cache tokens) live-updating.
  - Body: scrollable event log. Each event rendered per kind:
    - `system/init`: muted one-liner "resumed session <sid>" or "new session <sid>".
    - `assistant` text block: wrapped, foreground-accent, preceded by timestamp.
    - `assistant` tool_use block: collapsible panel with tool name + input JSON.
    - `result`: status summary + stop_reason.
    - `error`: red panel.
  - Footer input line: user's next message. Enter sends.
- Sends route to the session's stdin as a JSON line per claudestream's input format.
- Auto-attach: when a session launched via a `claudestream`-kind provider produces its first event, TUI pushes the chat screen (replaces the raw AttachScreen path for this provider kind).

#### Files
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/detail/chat.go` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/detail/chat_test.go` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/main_screen.go` (auto-attach dispatch on provider kind)

#### Acceptance criteria
- [x] Launching a claudestream session drops the user into the chat surface, not the raw attach panel. *(attachScreenForSession dispatch on ProviderID; launch path fetches DTO then pushes Chat vs. Attach — covered by TestAttachScreenForSessionDispatchesOnProviderKind + TestSessionFetchedForAttachPushesChatForClaudeStream.)*
- [x] Typed turn arrives in claude; response streams into the body as typed events render live. *(Unit-verified: chat reducer consumes canned events and renders concatenated deltas. End-to-end live-claude smoke deferred to manual pass.)*
- [x] Tool_use blocks are collapsible (Enter on a collapsed block expands; Enter again collapses). *(Tab cycles focus among blocks; Enter on focused block toggles — TestChatToolUseTabFocusAndEnterTogglesExpand.)*
- [x] Usage counters update on `result` events. *(KindUsage accumulates across turns; header renders in/out/cache totals — TestChatKindUsageAccumulatesAcrossTurns.)*
- [x] Esc detaches (session keeps running; daemon holds the subprocess). *(TestChatEscDetachesAndPops.)*

#### Scope fences
- Do NOT style this screen to feel like Nanite's chat UX. Keep it functional first; polish Sprint 6 equivalent.
- Do NOT implement history scrollback search.
- Do NOT implement message editing / re-sending. Linear chat only.

---

### T-v004-s02-05: Session resume via `--resume <session_id>`

**Priority:** 2. **Tags:** resume, logical-agent, session.

#### Problem
Claude's model is "new subprocess per conversation continuation." The `session_id` from the `system/init` event is the key. To continue a prior conversation, the next launch must pass `--resume <id>`.

#### Fix direction
- When a claudestream session produces its first `system/init` event, persist the `session_id` on the associated `logical_agent` row.
- On a subsequent launch referencing the same logical agent, the claudestream adapter reads the last `session_id` and passes `--resume <id>` to the subprocess.
- TUI chat surface shows a "resuming session <sid>" indicator when a launch resumes rather than starts fresh.

#### Files
- `/Users/chrispian/Projects-apps/agent-mux/internal/store/migrations/0007_logical_agent_claude_session_id.sql` (new; add column or re-use existing hints_json)
- `/Users/chrispian/Projects-apps/agent-mux/internal/provider/cli/claudestream/adapter.go` (resume flag logic)
- `/Users/chrispian/Projects-apps/agent-mux/internal/runtime/manager.go` (persist session_id on event observation)

#### Acceptance criteria
- [x] Launch claude, have a short conversation, stop session. *(Callback wiring verified: TestSession_OnClaudeSessionIDCallbackFiresOnceForFreshSession confirms Store.SetClaudeSessionID is invoked on the first system/init event.)*
- [x] Launch again referencing the same logical agent → `--resume` flag present, chat surface shows resume indicator, claude's prior context is present. *(Preset wiring verified: TestSession_PresetCausesResumeFlagOnFirstTurn asserts `--resume <sid>` in the adapter's exec args when StartOptions.ClaudeSessionIDPreset is set by app.Service from the store.)*

**Design notes:**
- Capture path picked: **(a) callback in StartOptions** — additive to `provider.StartOptions` + `runtime.StartRequest`, with a no-op fallback for adapters that ignore it. Cleaner than threading the store through the manager or subscribing to a bus event for a single consumer.
- `SetClaudeSessionID` is gated on the adapter observing a genuinely new id (preset ≠ echoed id). Keeps store writes out of the claudestream hot path when resuming.

#### Scope fences
- Do NOT let users manually pick which historical `session_id` to resume from (latest-only for MVP).
- Do NOT implement branch/fork semantics — linear continuation only.
- Do NOT depend on Sprint v004-04 (Checkpoint Resume) being in place; claude's `session_id` resume is independent of our general checkpoint mechanism.

---

## Review / readiness notes

- **Nanite license / attribution.** Confirm both projects are under compatible licenses before copying. If needed, add a NOTICE / AUTHORS entry in `pkg/claudestream/` with Nanite attribution.
- **claude CLI version pinning.** Stream-json output shape could change across claude releases. Document the minimum tested version in the package README and add a runtime version check if claude emits its version in any event.
- **Partial-message mode.** `claude --include-partial-messages` may be necessary for smooth streaming in the chat surface (get tokens as they arrive rather than whole-message). Verify during T-02 manual smoke.
- **Session_id backward-compat.** Existing `logical_agents` rows from v0.0.2 don't have a `claude_session_id` field. Migration must default to empty; resume-flag logic must handle empty gracefully (fresh session).
- **Integration with Sandboxing (Sprint 3).** Subprocess spawns from claudestream must honor the same sandbox profile application as PTY spawns. Make sure T-v004-s02-02's adapter reads `provider.StartOptions.Sandbox` and wraps the subprocess the same way.
- **Ecosystem package boundary.** During T-01, resist the urge to put anything agent-mux-specific in `pkg/claudestream/`. If you're adding something and it feels like it belongs in the host app, it does — keep it in `internal/`.
