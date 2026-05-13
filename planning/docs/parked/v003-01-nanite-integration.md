# Sprint v003-01 — Nanite Integration

Epic: [[Parked] Integration Foundation](./v0.0.3-integration-foundation.md)

**Epic:** [[Parked] Integration Foundation](./v0.0.3-integration-foundation.md)
**Goal:** Make Nanite a first-class client of the mux daemon. Nanite should discover the daemon, list sessions, attach/detach without killing, and send input round-trip. The goal is a working integration, not a finished UX.
**Exit criteria:**
- [ ] Nanite can call `GET /sessions` and render a session list.
- [ ] Nanite can attach to a running session via SSE and display live output.
- [ ] Nanite can send input to a running session.
- [ ] Detaching Nanite does not kill the session; re-attaching resumes correctly (with `since_seq` where available).

## Context

Context-pack §01 positions Nanite as the interactive UX client ("chat UX, session browsing/attach, tool-centric workflows, context review"). Context-pack §03 v0.0.3 bullets include "Nanite can discover and attach to Agent Mux sessions." Context-pack §04 use case 1 is the canonical Nanite scenario: interactive attached coding session with detach/reattach.

Current state:
- Mux API from Sprint v002-05 exposes the endpoints Nanite needs.
- Nanite is a separate repo not visible to this planning pass. The execution agent must inspect the Nanite codebase before breaking this sprint into real tasks.

This sprint is intentionally thin at capture time. Readiness review is expected to refine once the Nanite repo is in hand.

## Tasks

### T-v003-s01-01: Discover mux daemon from Nanite

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [integration, nanite, discovery]

#### Problem

Nanite doesn't know how to find a running mux daemon. Possible approaches: fixed address from Nanite config, PID file lookup, environment variable.

#### Fix direction

- Define a minimal discovery contract: Nanite looks for `$MUX_ADDR` env var first; falls back to `$XDG_RUNTIME_DIR/agent-mux/muxd.sock`; falls back to `127.0.0.1:7180`.
- Implement a small `mux-client` module inside Nanite (or a shared package) that probes endpoints and returns a configured client.
- Surface clear error UX in Nanite if daemon isn't reachable ("Is `mux daemon` running?").

#### Files

- Nanite repo — path TBD once repo is inspected.
- Possibly `pkg/` or `internal/client/` in the mux repo if the client gets extracted as a library.

#### Acceptance criteria

- [ ] Nanite resolves the daemon address via the documented precedence.
- [ ] With daemon up, Nanite's health check succeeds.
- [ ] With daemon down, Nanite displays a clear error.

#### Test plan

- Manual: start/stop daemon with Nanite running, verify UX.
- Unit (Nanite side): mock daemon responses.

#### Scope fences

- Do not add remote-daemon support. Discovery is local-only.
- Do not reimplement client logic in Nanite if a shared package is cleaner — propose extraction.

#### Relationship

Blocks: T-v003-s01-02, T-v003-s01-03.

#### Origin

Context-pack [03-vfuture-roadmap.md](../agent-mux-vfuture-context-pack/03-vfuture-roadmap.md) (v0.0.3 "Nanite can discover and attach"), [04-use-cases.md](../agent-mux-vfuture-context-pack/04-use-cases.md) use case 1.

---

### T-v003-s01-02: Session list + attach in Nanite

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [integration, nanite, attach, ui]

#### Problem

Nanite has no session-list view wired to mux, and no attach stream rendering.

#### Fix direction

- Add a Nanite UI panel (or reuse existing) that renders `GET /sessions` and polls / subscribes to `GET /events/stream` with `scope=session` for updates.
- On session selection, open an attach panel that consumes `GET /sessions/{id}/attach` SSE.
- Render PTY output with ANSI handling (Nanite likely already has this for its own chat transcripts).

#### Files

- Nanite repo — path TBD.

#### Acceptance criteria

- [ ] Session list in Nanite reflects mux's state live.
- [ ] Selecting a session opens an attach pane that streams output.
- [ ] Closing the pane detaches without killing.

#### Test plan

- Manual end-to-end: launch via `mux launch`, attach in Nanite, interact.

#### Scope fences

- Do not re-implement mux features inside Nanite (session state model, lifecycle logic).
- Do not add multi-session workspace/tab UX — that's v0.3+ rich-UX territory.

#### Relationship

Depends on: T-v003-s01-01.
Blocks: T-v003-s01-03.

#### Origin

Context-pack [04-use-cases.md](../agent-mux-vfuture-context-pack/04-use-cases.md) use case 1.

---

### T-v003-s01-03: Send input round-trip from Nanite

**kind:** agent
**priority:** 2
**manual:** true
**tags:** [integration, nanite, input]

#### Problem

Once attach works, the remaining leg is input: Nanite's input box should deliver bytes to the session.

#### Fix direction

- Wire Nanite's input widget to `POST /sessions/{id}/input`.
- Respect the JSON envelope format decided in Sprint v002-05.
- Echo local state handling: display what was sent immediately or rely on the PTY echo? Lean on the PTY echo (simpler, consistent with what a real terminal does).

#### Files

- Nanite repo — path TBD.

#### Acceptance criteria

- [ ] Typing in Nanite's input box and pressing Enter delivers the line to the session.
- [ ] The session's response arrives via the attach stream and renders in the UI.
- [ ] No double-echo (avoid rendering local input before server echo).

#### Test plan

- Manual: full round-trip with a shell session.

#### Scope fences

- Do not implement command history, autocomplete, or editor integrations here.
- Do not add binary-safe input (paste of large blobs) beyond what SendInput supports.

#### Relationship

Depends on: T-v003-s01-02.

#### Origin

Context-pack [04-use-cases.md](../agent-mux-vfuture-context-pack/04-use-cases.md) use case 1.

## Review / readiness notes

- **Nanite repo inspection is a prerequisite.** Before this sprint starts, an execution agent needs access to the Nanite codebase to identify where session-list and attach panels belong. Capture this as a readiness gap.
- **Client library extraction:** if Nanite and Clockwork both end up needing the same client, lift it into `github.com/chrispian/agent-mux/pkg/client` (context-pack §07 allows `pkg/` for external consumers). Decide after Sprint v003-02 scopes are clearer.
- **Discovery precedence:** the proposal ($MUX_ADDR / UDS / default HTTP) is a suggestion, not locked. Revisit with readiness.
