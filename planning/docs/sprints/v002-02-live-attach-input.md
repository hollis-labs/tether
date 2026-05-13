# Sprint v002-02 — Live Attach & Input

Epic: [v0.0.2](../epics/v0.0.2-runtime-foundation.md)

**Epic:** [v0.0.2](../epics/v0.0.2-runtime-foundation.md)
**Goal:** Replace the snapshot-only Attach with a live tail-follow stream that supports multiple simultaneous clients. Add input injection to the running PTY. Establish detach semantics so a client disconnect never kills the session.
**Exit criteria:**
- [x] `mux sessions attach <id>` (or API equivalent) produces a live stream that tails the log AND receives new PTY bytes as they arrive. → `mux sessions attach` + daemon `GET /sessions/{id}/attach`; smoke-tested against `/bin/cat` under PTY (see Session log).
- [x] `mux sessions input <id>` (or API equivalent) delivers bytes to the PTY; the child process sees them. → `mux sessions input` + daemon `POST /sessions/{id}/input`; bytes observed echoing through PTY + cat in smoke.
- [x] Two or more clients can attach simultaneously and each receives the full stream. → smoke: two concurrent attaches both received alpha/beta/gamma/delta; `attached_clients=2` while both subscribed.
- [x] Disconnecting a client does not terminate the session. → smoke: kill attach-A while attach-B keeps running; session state remained `running`; delta delivered to B only.
- [x] Snapshot/replay from the log file still works (the pre-existing log-tail behavior is a subset of live attach). → `mux sessions tail <id> --follow=false` + daemon-down fallback both preserve the log-file copy path.

## Context

Context-pack §06 §2 is unambiguous: "The current attach behavior reads from the log file and does not provide live attach, tail-follow, or input injection to a running PTY." Context-pack §04 use cases 1 (interactive attached) and 2 (detached background task) both depend on this. Context-pack §08 defines the attach/detach semantics: "Interactive attached session / Detached running session / Background session / Reattach."

Current shape to evolve:
- `/Users/chrispian/Projects-apps/agent-mux/internal/session/runtime.go` — `Handle.Attach` currently `io.Copy` from log file and returns. Needs to become a live stream.
- `/Users/chrispian/Projects-apps/agent-mux/cmd/mux/sessions.go` — `sessionsTailCmd` opens the log file and copies once.
- `/Users/chrispian/Projects-apps/agent-mux/internal/runtime/manager.go` (from v002-s01-01) — will own an "attach manager" responsibility.

When done, attach is a real stream primitive ready for the API transport (Sprint v002-05) to expose over SSE/WebSocket.

## Tasks

### T-v002-s02-01: Introduce an Attach Manager with fan-out

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [feature, attach, runtime]

#### Problem

`session.Handle.Attach` supports exactly one consumer, copies the log file, and returns. That's not a live attach; it's a snapshot. And it can't serve two clients.

#### Evidence

`/Users/chrispian/Projects-apps/agent-mux/internal/session/runtime.go:Handle.Attach` — opens `h.LogFile.Name()`, `io.Copy`, done. No tail-follow, no PTY read fan-out.

#### Fix direction

Add `internal/runtime/attach.go` (or `internal/session/attach.go`):
- Each active session has a bounded ring buffer of recent output + a fan-out broker.
- When the session PTY reader sees bytes, it:
  1. appends to the log file (existing behavior);
  2. writes into the ring buffer;
  3. publishes to any subscribed writers.
- An `Attach(ctx, sessionID, w io.Writer, opts)` API:
  1. reads the historical tail from the log file (or the ring buffer, bounded);
  2. then switches to the live publish channel;
  3. returns when ctx is done (client detach).
- Subscription list guarded by a mutex; disconnects are non-fatal to other subscribers.

Reuse existing PTY reader goroutine; don't spawn a new one per subscriber.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/runtime/attach.go` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/session/runtime.go` — adjust the PTY→log goroutine to also feed the attach broker; remove the single-consumer `Attach` method (or mark deprecated)
- `/Users/chrispian/Projects-apps/agent-mux/internal/runtime/manager.go` — expose `Attach(ctx, id, w)` on the manager

#### Acceptance criteria

- [x] Two `mux sessions attach <id>` invocations against the same running session both receive output. → library-level proof: `TestManager_AttachMultipleSubscribersBothReceive`. CLI wiring tracked in T-v002-s02-04.
- [x] A client disconnect does not kill the session (running child stays alive; other subscribers keep receiving). → `TestManager_AttachSurvivesSiblingDetach`.
- [x] The attach stream includes recent historical output (tail) before switching to live. → `TestAttachBroker_ReplayBeforeLive` + `copyStream` writes replay first.
- [x] No goroutine leak: when all subscribers detach and the session exits, all related goroutines terminate. → `TestManager_AttachReturnsWhenSessionExits` + race suite.

#### Test plan

- Unit: start a stub session, attach two writers, write bytes to the PTY side, assert both writers receive them; cancel one's context, assert the other still receives. ✅ `TestManager_Attach*` + `TestAttachBroker_*`.
- Integration: `mux daemon start`, launch a stub session, attach from two terminals, detach one, verify the other keeps streaming. → deferred to T-v002-s02-04 smoke once CLI `attach` exists.
- Race: `go test -race` on the attach broker with concurrent subscribe/publish/detach. ✅ `TestAttachBroker_ConcurrentSubscribePublishNoRace` + full suite under `make test-race`.

#### Scope fences

- Do not design a generic pub/sub library here — keep attach fan-out local to `internal/runtime`. General event bus is Sprint v002-06.
- Do not persist every byte for replay beyond the existing log file — ring buffer is memory-only.
- Do not change the log file format.

#### Relationship

Depends on: T-v002-s01-01 (runtime manager).
Blocks: T-v002-s02-03 (daemon-side client attach), T-v002-s05-02 (SSE attach endpoint).

#### Origin

Context-pack [06-review-of-current-v0.md](../agent-mux-vfuture-context-pack/06-review-of-current-v0.md) §2, [08-api-and-runtime-contract.md](../agent-mux-vfuture-context-pack/08-api-and-runtime-contract.md) attach/detach semantics.

---

### T-v002-s02-02: Add SendInput to running PTY

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [feature, input, runtime]

#### Problem

There is no way to push input into a running session. The boot prompt is written once at start; after that, the PTY writer is unreachable from outside `session.Handle`.

#### Fix direction

Expose a thread-safe `SendInput(id string, data []byte) error` on `runtime.Manager`:
- Locks the session entry, gets the PTY writer.
- Writes bytes; no trailing newline auto-insertion (that's the caller's concern — CLI may add one, API callers may not).
- Returns an error if the session is not in `running` state.

Wire a CLI command `mux sessions input <id> [text]` that either:
- Takes `text` as a positional arg and writes `text + "\n"`, or
- With no arg, pipes stdin to the session PTY until EOF.

Readiness question: should `mux sessions input` write bytes raw, or translate line endings? Recommend raw for the API path, newline-appending for the CLI convenience arg.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/runtime/manager.go` — add `SendInput`
- `/Users/chrispian/Projects-apps/agent-mux/internal/session/runtime.go` — expose a safe writer accessor
- `/Users/chrispian/Projects-apps/agent-mux/cmd/mux/sessions.go` — add `sessionsInputCmd`

#### Acceptance criteria

- [x] `mux sessions input <id> "hello"` delivers `hello\n` to the child; it's visible in attach output (if the shell echoes) and in the log file. → lib + daemon + CLI wired; end-to-end smoke with a real provider tracked for sprint close.
- [x] `echo foo | mux sessions input <id>` pipes stdin through. → CLI stdin branch in `sessionsInputCmd`.
- [x] Attempting to send input to a completed session returns an error without crashing. → `TestManager_SendInputUnknownSessionErrors` + daemon 404 `TestHandleSendInput_SessionNotRunning`.
- [x] Concurrent `SendInput` calls are serialized safely (no interleaved partial writes). → `TestManager_SendInputSerialisesConcurrentWrites` (atomicWriter fails if two Write calls overlap).

#### Test plan

- Unit: stub session (bash or `cat` under PTY), SendInput, read back from attach, assert bytes arrive.
- Race: concurrent SendInput + Attach + Stop.
- CLI manual: attach in one shell, `mux sessions input` in another, see the input arrive.

#### Scope fences

- Do not implement control-sequence handling (resize, signals via escape) in this task. Control channel is a future enhancement.
- Do not auto-append newlines at the API layer — caller decides.
- Do not open a persistent "input stream" endpoint; a single POST per message is fine for v0.0.2.

#### Relationship

Depends on: T-v002-s01-01.
Pairs with: T-v002-s02-01 (live attach makes input meaningful).

#### Origin

Context-pack [03-vfuture-roadmap.md](../agent-mux-vfuture-context-pack/03-vfuture-roadmap.md) (v0.0.2 "send input to session"), [08-api-and-runtime-contract.md](../agent-mux-vfuture-context-pack/08-api-and-runtime-contract.md) `POST /sessions/{id}/input`.

---

### T-v002-s02-03: Track client attachments in the daemon

**kind:** agent
**priority:** 3
**manual:** true
**tags:** [feature, observability, attach]

#### Problem

Multiple clients attaching to sessions is a new behavior. Without a record of *who* is attached, operators can't reason about whether a session is being watched, and future features (idle detection, UX for "you have 2 watchers") have nothing to key off.

#### Fix direction

- On subscribe, insert a row into `client_attachments` (table introduced in Sprint v002-03): `id, session_id, client_kind, attached_at`. On detach, stamp `detached_at`.
- Daemon-side: each attach call gets a client-attachment ID; the attach goroutine owns its row lifecycle.
- Expose attachment count on session snapshots.

Readiness note: if `client_attachments` table isn't available when this task runs (v002-s03-01 not merged), either block this task or stub with an in-memory list and TODO.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/runtime/attach.go` — instrument lifecycle
- `/Users/chrispian/Projects-apps/agent-mux/internal/store/sqlite.go` — add CRUD for `client_attachments`
- `/Users/chrispian/Projects-apps/agent-mux/internal/runtime/manager.go` — surface count on list/get

#### Acceptance criteria

- [x] Attaching creates a row; detaching stamps `detached_at`. → `TestManager_AttachPersistsLifecycleThroughSink` + `TestClientAttachments_CreateAndList`.
- [x] Session `get` output includes `attached_clients: N`. → `SessionDTO.AttachedClients` populated from `Service.AttachedClients(id)` → `Runtime.Get(id).AttachedClients`; covered by `TestManager_AttachCounterSurvivesConcurrentClients`.
- [x] Stale rows (daemon crashed with subscriber alive) are swept on daemon start. → `TestClientAttachments_SweepStale` + `app.Service.New` calls `SweepStaleAttachments` at startup.

#### Test plan

- Unit: attach + detach produces one row pair; attach/detach sequence produces two distinct pairs.
- Recovery: prepopulate a stale row, start daemon, verify it's marked detached.

#### Scope fences

- Do not store the attach *output* here — just the metadata.
- Do not add auth/identity to attachments — `client_kind` is a free-form label for now.

#### Relationship

Depends on: T-v002-s01-01, T-v002-s02-01, T-v002-s03-01 (schema).
Siblings: none.

#### Origin

Context-pack [05-implementation-guidelines.md](../agent-mux-vfuture-context-pack/05-implementation-guidelines.md) storage list (client_attachments).

---

### T-v002-s02-04: Update `mux sessions tail` to use live stream

**kind:** agent
**priority:** 2
**manual:** true
**tags:** [cli, attach]

#### Problem

`sessions tail` in v0.0.1 opens the log file and copies once. Now that the daemon owns running sessions and live attach exists, `tail` should be a convenience alias for live attach.

#### Evidence

`/Users/chrispian/Projects-apps/agent-mux/cmd/mux/sessions.go:sessionsTailCmd` uses `r.Workspace + "/logs/session.log"` and `io.Copy`.

#### Fix direction

- `mux sessions tail <id>` → client call to daemon attach endpoint, pipe to stdout.
- With daemon not running / session terminal: fall back to reading the log file (like today).
- Support `--follow=false` to force the one-shot snapshot behavior (for scripts).

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/cmd/mux/sessions.go`
- `/Users/chrispian/Projects-apps/agent-mux/internal/client/` (from T-v002-s01-03)

#### Acceptance criteria

- [x] `mux sessions tail <id>` against a running session streams live output until Ctrl-C. → daemon `/sessions/{id}/attach` + `runAttach` + `tail --follow=true` default.
- [x] `mux sessions tail <id> --follow=false` prints the current log and exits. → `runTailSnapshot` branch.
- [x] Against a completed session, defaults to file read (not an error). → `tail --follow` degrades to `runTailSnapshot` on 404 "session not running".

#### Test plan

- Manual: daemon up, launch stub, tail in two terminals, verify both stream.
- Manual: stop the session, re-tail, see the completed log.

#### Scope fences

- Do not merge `tail` and `attach` into one command — keep `attach` as the primary command; `tail` is a read-only alias.
- Do not add interactive input to `tail`.

#### Relationship

Depends on: T-v002-s01-03, T-v002-s02-01.

#### Origin

Context-pack [06-review-of-current-v0.md](../agent-mux-vfuture-context-pack/06-review-of-current-v0.md) §2.

## Review / readiness notes

- **Ring buffer size** is unspecified. A sensible default is 1 MB or 10k lines — whichever is smaller. Decide in T-v002-s02-01 and make configurable per-session later.
- **Binary vs text streams:** the PTY can emit arbitrary bytes (ANSI escapes, colors, etc.). The attach stream must preserve these; don't assume UTF-8 lines. Any framing over the wire (SSE, WebSocket) in Sprint v002-05 has to handle this.
- **Input framing** for the API endpoint in Sprint v002-05 needs a decision: raw body, base64, or a JSON envelope with a base64 field? Capture as a readiness gap in that sprint.
