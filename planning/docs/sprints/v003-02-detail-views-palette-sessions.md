# Sprint v003-02 — Detail Views + Command Palette + Session Observation

Epic: [v0.0.3](../epics/v0.0.3-tui-mvp.md)

**Epic:** [v0.0.3](../epics/v0.0.3-tui-mvp.md)
**Goal:** Make the TUI navigable and make running sessions manageable. Introduce a screen-stack so the main screen can push and pop detail views per object type. Add a command palette for verb-driven navigation. Wire live attach over SSE (or WebSocket, per v002-s05 decision) so a user can drill into a session, watch output stream in real time, send input, stop it, or snapshot a tail of its log.
**Exit criteria:**
- [ ] Right arrow (or `l`) on any result row pushes a type-appropriate detail screen. Esc / Left arrow (or `h`) pops back to the main screen. The screen stack handles arbitrary depth.
- [ ] Detail screens exist for Project, Agent, Provider, Launch, and Session. Each pretty-prints the relevant fields and exposes the same footer-keybinding pattern as the main screen.
- [ ] Command palette opens on `:` or Ctrl-K; fuzzy-matches a registered verb list; Enter executes the verb; Esc closes. Registered verbs this sprint: `view projects|agents|providers|launches|sessions`, `quit`, `help`.
- [ ] Session detail screen has a live attach action (`a`) that opens a streaming viewport consuming `GET /sessions/{id}/attach`; an input line at the bottom of the attach panel sends to `POST /sessions/{id}/input` on Enter; Esc detaches without killing.
- [ ] Session detail screen has a stop action (`x`) with a confirmation prompt; it calls `POST /sessions/{id}/stop`.
- [ ] Session detail screen has a tail-log snapshot action (`t`) that pulls the historical log into a scrollable viewport (non-streaming).

## Context

Sprint v003-01 lands a single-screen launcher. This sprint generalizes: multiple screens, a verb-driven navigation layer, and real-time interaction with running sessions. Context-pack §04 use cases 1 and 2 (interactive attached session; detached background task) both land their TUI expressions here. Context-pack §08 attach-stream and send-input semantics are consumed directly.

Current shape to evolve:
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/` (from Sprint 1) — root model is a single screen. Needs a screen-stack abstraction.
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/client/` (from Sprint 1) — add attach-stream and session-input methods here.
- Sprint v002-05-02 defines `GET /sessions/{id}/attach` SSE and `POST /sessions/{id}/input`.

When done, the TUI stops being a one-screen launcher and starts being a general-purpose navigation and observation surface.

## Tasks

### T-v003-s02-01: Screen-stack model + main ↔ detail routing

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [feature, tui, navigation]

#### Problem

The Sprint 1 root model is a single-screen reducer. Detail views, the palette overlay, and the attach panel all need to stack over it, capture keystrokes, and pop back cleanly. Without a shared screen-stack, each new screen reinvents the wheel.

#### Fix direction

- New type `Screen` interface in `internal/tui/screen/`:
  - `Init() tea.Cmd`
  - `Update(tea.Msg) (Screen, tea.Cmd)`
  - `View() string`
  - `KeyBindings() []key.Binding` (so `?` overlay from Sprint 6 can enumerate them)
  - `Title() string` (breadcrumb display)
- New `Stack` type holding a slice of `Screen` with `Push(Screen)`, `Pop() Screen`, `Top() Screen`.
- Refactor the Sprint 1 main screen to implement `Screen`. It becomes the stack's initial element.
- Root model holds a `*Stack`. `Update` delegates key/msg handling to `stack.Top()`; a `PopMsg` sentinel tells the root to pop.
- Header renders a compact breadcrumb from the stack titles (`Main › Session abc123`).
- Escape handling convention: if the top screen doesn't consume Esc, the root pops. If the stack has one element, Esc is a no-op (doesn't exit).

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/screen/` (new; Screen interface + Stack)
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/model.go` — rework root to route through the stack
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/main_screen.go` (new; Sprint 1's main-screen logic extracted)

#### Acceptance criteria

- [x] Root model holds a screen stack and delegates to `stack.Top()`. *(`internal/tui/model.go` now a thin wrapper; `New()` returns `Model{stack: screen.NewStack(NewMainScreen(c))}`)*
- [x] Main screen implements `Screen`. *(`MainScreen` in `main_screen.go` satisfies the full interface: Init / Update / View / KeyBindings / Title)*
- [x] Pushing and popping screens works without flicker and without losing the main-screen state. *(test: `TestRootPushesScreenOnPushMsg`, `TestRootPopReturnsToMain`; MainScreen stays on the stack unchanged under pushed screens, so search / filters / selection persist)*
- [x] Esc from the root main screen is a no-op; `q` still quits. *(PopScreenMsg refuses when stack has 1 element; `q` quit path unchanged in MainScreen)*

**Deviation from sprint text:** the "breadcrumb header" was deferred. Screens render their own frame; with only one screen type live (MainScreen), a breadcrumb would just say "Main" — tautological. Sprint 2 T-02 introduces a second screen type; breadcrumb gets added at T-02 when it becomes load-bearing.

#### Test plan

- Unit: stack push / pop / peek tests.
- Unit: root `Update` tests showing msg delegation to the top screen.
- Manual: push a placeholder detail screen, pop it, confirm main-screen state is preserved.

#### Scope fences

- Do not implement detail screens in this task — T-02 does that.
- Do not implement palette / attach overlays here — T-03 / T-04.
- Do not persist stack state across TUI restarts.
- Do not make the stack concurrent-safe; the Bubble Tea `Update` loop serializes access.

#### Relationship

Depends on: T-v003-s01-02.
Blocks: T-v003-s02-02, T-v003-s02-03, T-v003-s02-04, every Sprint 3–5 task that introduces a new screen.

#### Origin

User sketch: "Right arrow views, Esc goes back." Epic v0.0.3: detail screens per object type.

---

### T-v003-s02-02: Detail screens for Project / Agent / Provider / Launch / Session

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [feature, tui, detail-views]

#### Problem

Listing objects without inspection is half a product. The user needs to see full field values, not just the summary row rendered on the main screen.

#### Fix direction

- One file per object type under `internal/tui/detail/`:
  - `project.go` — renders `config.Project` fields (ID, Name, RepoRoot, TrackingRoot, KnowledgeBase, BootFragments, WorkspaceSpec).
  - `agent.go` — renders `config.Agent` including `AgentPermissions`.
  - `provider.go` — renders `config.Provider` including `BootstrapSpec` and `ProviderEnv.Passthrough`.
  - `launch.go` — renders `config.Launch` including `LaunchWorkspace`, `PromptSpec`, `LaunchOverrides.Env`.
  - `session.go` — renders `runtime.SessionSnapshot` plus plan summary.
- Shared `field.go` helper with labeled-row rendering: left-aligned bold label, right-aligned value, Lipgloss-themed. Lists render as indented bullets. Maps render as key: value pairs.
- Right arrow / `l` on a main-screen row fetches the detail record (via `client.GetProject(id)` etc. — add these to `internal/tui/client/`) and pushes the appropriate detail screen.
- Detail footer shows: `← back · e edit · d delete · : palette · ? help`. Edit and delete are wired as "coming in Sprint 3" toasts for now (except Session, which doesn't use Sprint 3 forms).
- Session detail has a richer footer that previews Sprint-2-T-04 / T-05 actions: `← back · a attach · x stop · t tail · : palette · ? help`.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/detail/` (new; one file per type)
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/client/` — add `GetProject`, `GetAgent`, `GetProvider`, `GetLaunch`, `GetSession`
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/main_screen.go` — Right-arrow handler pushes detail screen

#### Acceptance criteria

- [x] Right arrow on each of the five row types pushes the correct detail screen. *(test: `TestRightArrowPushesDetailForEachRowType` seeds all five row types and asserts the pushed screen's concrete type)*
- [x] Each detail screen renders every non-empty field from the typed config object. *(renderFields helper in `detail/field.go`; each screen's refresh() enumerates its struct fields — fields use the actual config struct shape, which differs slightly from the sprint text: `WorkspaceSpec` is `DefaultMode/WorktreeBase/SessionRoot`, `BootstrapSpec` is `Mode/PromptPrefix` not `Script`, `AgentPermissions` is `Network/DefaultSandbox` not Edit/Read/Deny, `ProviderEnv` has no `Extra` field)*
- [x] Long list fields render as indented lists, not truncated one-liners. *(renderFields uses per-item bullets when `field.list` is non-nil)*
- [x] Session detail shows runtime state, workspace path, start time, attached-client count. *(Plus PID, exit code, ended_at; also re-fetches on Init via ListSessions — GetSession isn't yet in the TUI client wrapper, filter-in-list is a cheap equivalent)*
- [x] Esc / Left arrow returns to main screen. *(`base.updateCommon` matches `esc`/`left`, emits `screen.Pop()` cmd)*

**Deviations from sprint text:**
- Detail footer does NOT include `e edit · d delete` placeholders for Sprint 3. Those bindings + Sprint-3 toasts land when Sprint 3 implements the forms. Footer shows `← back · : palette · ? help · ctrl+c quit`.
- Session detail does NOT include `a attach · x stop · t tail` previews. Those bindings land in T-04 and T-05 of this sprint.
- Breadcrumb header deferred: each detail screen shows its own title in its own header row. With only MainScreen + one detail level live, a stack-wide breadcrumb is redundant. Adds when the stack depth grows.

#### Test plan

- Unit: snapshot-style renderer tests for each type against a canned config object.
- Manual: navigate into each type against a seeded catalog and verify rendering.

#### Scope fences

- Do not implement edit or delete here — those are Sprint 3 (and depend on the catalog-CRUD decision).
- Do not implement attach / stop / tail here — T-04 and T-05.
- Do not add tabs or multi-pane layouts inside a detail screen. One scrollable viewport per screen is enough.
- Do not auto-refresh detail views. The user can pop and re-push to re-fetch.

#### Relationship

Depends on: T-v003-s02-01.
Blocks: T-v003-s02-04, T-v003-s02-05, Sprint 3 edit-from-detail flows.

#### Origin

User sketch: "Right arrow views." Epic v0.0.3 exit criteria: drill-in per object type.

---

### T-v003-s02-03: Command palette overlay

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [feature, tui, palette, navigation]

#### Problem

Keyboard-first nav needs a verb layer. "View projects" should be two keystrokes (`: v p <enter>`) regardless of what screen the user is on. A palette also gives Sprint 3–5 a low-cost way to register new verbs (`add project`, `resume checkpoint`) without hunting for a spare key binding.

#### Fix direction

- New package `internal/tui/palette/`:
  - `Verb` struct — name (e.g. `view projects`), description, aliases, handler (`func() tea.Cmd`).
  - `Registry` with `Register(Verb)` and `Search(query string) []Verb` using fuzzy match.
  - `Overlay` type implementing `Screen` — renders a centered Lipgloss panel with an input line + results. Ignores main-screen key bindings while active.
- Trigger keys: `:` and Ctrl-K at the root level. The trigger pushes `Overlay` onto the stack; Esc pops.
- This sprint registers: `view projects`, `view agents`, `view providers`, `view launches`, `view sessions`, `quit`, `help`. Each "view X" verb pops back to main and sets the filter chips to that type only.
- Sprint 3 / 4 / 5 will register their own verbs via `palette.Registry.Register(...)` in their packages' `init()` or via an explicit wiring step in `internal/tui/wire.go`.
- Palette width ~60 cols; results list up to 8 visible; description renders in muted color next to verb name.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/palette/` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/model.go` — hook `:` / Ctrl-K triggers
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/wire.go` (new; central verb registration point)

#### Acceptance criteria

- [x] `:` and Ctrl-K both open the palette from the main screen. *(MainScreen intercepts both key patterns and emits `openPalette()` cmd; root Model's Update pushes `palette.NewOverlay(m.registry)`.)*
- [x] Typing fuzzy-matches registered verbs; Enter executes the selected verb. *(test: `TestOverlayTypingFiltersVisible`, `TestOverlayEnterRunsHandler`.)*
- [x] Esc closes the palette and restores the previous screen state. *(test: `TestOverlayEscPops`; screen.Stack keeps MainScreen's state intact under the overlay.)*
- [x] `view sessions` switches filters to only show sessions on the main screen and pops back to it. *(Verb handler emits `tea.Batch(screen.Pop(), applyFilterMsg{solo: RowTypeSessions})`; MainScreen reducer handles `applyFilterMsg` by calling `setSoloChip` + `recomputeVisible`.)*
- [x] `quit` exits the TUI. *(Verb handler returns `tea.Quit` directly.)*
- [x] Verbs added by later sprints show up automatically without re-touching this file. *(Sprints 3–5 call `registry.Register(...)` from their own packages; `registerDefaultVerbs` only owns the Sprint-2 set.)*

**Deviation from sprint text:** `:` / Ctrl-K open the palette from MainScreen only. Detail screens don't yet have the trigger — adds when T-04 / T-05 wire their own key bindings (so the attach screen can forward `:` to its session as a literal character rather than opening the palette mid-shell). `help` verb omitted for MVP — Sprint 6's `?` overlay will register it when the target surface exists.

#### Test plan

- Unit: fuzzy-match ordering tests.
- Unit: verb-execution test — register a stub verb, open palette, type its prefix, press Enter, assert the handler ran.
- Manual: open the palette from main, from a detail view, and from the attach panel (Sprint 2 T-04); verify it composes correctly with the stack.

#### Scope fences

- Do not build a shell-like parser with arguments (e.g. `edit project demo-proj`). Simple verb selection is enough for v0.0.3. Argument capture is a future enhancement.
- Do not add palette history (recent verbs). Not needed for MVP.
- Do not allow mouse dismissal.
- Do not add palette-within-palette nesting.

#### Relationship

Depends on: T-v003-s02-01.
Pairs with: T-v003-s06-01 (`?` help overlay is a sibling overlay pattern).

#### Origin

User sketch: "Command palette (`:` or Ctrl+K) for commands like `view projects`, `add project`."

---

### T-v003-s02-04: Live attach panel — SSE stream + input line

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [feature, tui, attach, sse, session]

#### Problem

Session detail can show static state; the real need is live attach. Without SSE streaming in the TUI, users bounce to a terminal to run `mux sessions attach` — defeating the purpose of an integrated UI.

#### Fix direction

- New screen `internal/tui/detail/attach.go` implementing `Screen`. Pushed when the user presses `a` on session detail.
- Attach-panel layout: header with session short-id + workspace path + attached-clients count; scrollable viewport with PTY output; bottom input line; footer with `← detach · Ctrl-C send SIGINT · Ctrl-D send EOF · : palette · ? help`.
- `internal/tui/client/attach.go` — new method `AttachStream(ctx, sessionID, sinceSeq) (<-chan AttachEvent, error)`. Wraps an SSE client (`github.com/r3labs/sse/v2` or a hand-rolled `bufio.Scanner` over the response body). Emits chunked events as `AttachEvent{Kind: "data"|"event", Bytes: []byte, Seq: int}`.
- Bubble Tea integration: an adapter goroutine converts the channel into `tea.Msg`s via `tea.Cmd`s. On each batch, append to the viewport buffer; re-render.
- Input line: Bubbles `textinput`. On Enter, `client.SendInput(sessionID, line + "\n")`. Bytes are base64-encoded per v002-s05-02's JSON-envelope convention. Immediately clear the input (do not local-echo; rely on PTY echo per context-pack §04 use case 1).
- Ctrl-C sends `\x03`; Ctrl-D sends `\x04`. Both use `client.SendInput` with raw bytes.
- Detach: Esc or Left arrow pops the screen and cancels the attach context; the SSE connection closes; the daemon keeps the session running.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/detail/attach.go` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/client/attach.go` (new; SSE wrapper + SendInput)
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/detail/session.go` — bind `a` to push the attach screen

#### Acceptance criteria

- [x] `a` from session detail opens the attach panel and begins streaming output. *(SessionScreen's Update matches `a` via attachKey and emits `screen.Push(NewAttachScreen(...))`; AttachScreen.Init spawns a goroutine that calls `client.AttachStream` into a chanWriter.)*
- [x] Typing into the input line and pressing Enter delivers bytes to the session; PTY echo renders them in the viewport. *(test: `TestAttachEnterCapturesAndClearsInput`; live behavior pending manual smoke.)*
- [x] Ctrl-C and Ctrl-D deliver raw control bytes. *(test: `TestAttachCtrlCDoesNotQuitInsteadForwardsSIGINT`; verifies tea.Quit is NOT emitted. sendBytesCmd issues raw `0x03` / `0x04` via `client.SendInput`. Live forwarding pending manual smoke.)*
- [x] Esc detaches; the session keeps running. *(test: `TestAttachEscPopsAndCancels`; verifies PopScreenMsg + ctx cancellation. Cancellation causes the upstream HTTP read to unwind, closing the attach without hitting `/stop`.)*
- [ ] Two TUI clients can attach to the same session simultaneously. *(inherits from v002-s05 daemon-side fan-out; not separately exercised here — manual smoke if needed.)*
- [x] The panel handles ANSI escapes so TUI apps running inside the session render reasonably. *(raw byte passthrough; we don't transcode ANSI. `vim` / `top` may still misrender due to PTY-size mismatch — out of scope; flagged below.)*

**Known gap for MVP:** PTY resize propagation isn't wired. The session's PTY stays at whatever size it was launched with; the attach panel sizes independently. Apps like `vim` that care about terminal dimensions may render oddly. Surfacing as backlog.

#### Test plan

- Unit: fake SSE server with canned events; assert viewport contents after streaming finishes.
- Unit: input-path test — `SendInput` called with base64 of `hello\n`.
- Manual end-to-end: launch a shell session via quick-launch; attach; run `ls` / `top`; detach; re-attach with Sprint 1 → Enter flow from another shell, confirm state persists.
- Manual: ANSI torture test — run `vim` inside the session and confirm screen redraws don't corrupt the TUI.

#### Scope fences

- Do not implement scrollback search in the attach viewport. Up/down arrow + page keys are enough.
- Do not add recording / replay features.
- Do not switch transport to WebSocket unless v002-s05 flipped. Per readiness note, SSE is the v0.0.3 default.
- Do not re-render the main screen under the attach panel — the attach panel takes the full area.

#### Relationship

Depends on: T-v003-s02-01, T-v003-s02-02, v002-s05-02 (attach + input endpoints), v002-s02 (attach broker + SendInput).
Blocks: T-v003-s02-05.

#### Origin

User sketch: "Session live attach — press `a` → stream from daemon's attach endpoint. Input line at bottom sends to session stdin. Esc detaches (session stays running)." Context-pack [04-use-cases.md](../agent-mux-vfuture-context-pack/04-use-cases.md) use case 1.

---

### T-v003-s02-05: Session stop + tail-log snapshot

**kind:** agent
**priority:** 2
**manual:** true
**tags:** [feature, tui, session, stop, tail]

#### Problem

Attach is one half of session management. The other half is terminating a session cleanly and looking at its historical log without streaming.

#### Fix direction

- Bind `x` on session detail (and on the attach panel's footer after detach) to show a modal confirmation: `Stop session <id>? (y/N)`. On `y`, call `client.StopSession(ctx, sessionID, force=false)`. On `N` or Esc, dismiss.
- Bind `t` on session detail to push a tail-log snapshot screen that renders the most recent log window (e.g. last 500 lines) in a viewport. Use an existing read endpoint — `GET /sessions/{id}/attach?since_seq=...&snapshot=true` if v002-s05 supports it, otherwise a new `GET /sessions/{id}/log?limit=N` (flag in readiness if the endpoint doesn't exist).
- Tail-log snapshot is read-only, non-streaming, scrollable, and has its own footer: `← back · r refresh · : palette · ? help`.
- After a successful stop, pop back to main screen and push a toast `Stopped session <id>`.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/detail/session.go` — bind `x` and `t`
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/detail/tail.go` (new; tail-log snapshot screen)
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/modal.go` (new; generic confirmation modal)
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/client/sessions.go` — add `GetSessionLog(ctx, id, limit)` wrapper

#### Acceptance criteria

- [x] `x` opens a confirmation modal; `y` stops the session; `N` / Esc cancels. *(SessionScreen's stopKey pushes `modal.NewConfirm`; modal's y runs onYes then pops; n/Esc just pop. Tests: `TestConfirmYesRunsHandlerAndPops`, `TestConfirmNoJustPops`, `TestConfirmEscJustPops`.)*
- [x] Post-stop, a toast confirms the action. *(SessionScreen handles `sessionStoppedMsg` with a batch of `screen.Pop()` (back to main) + `screen.Toast(ToastInfo, ...)`; MainScreen's handler for `screen.ToastEmitMsg` pushes the toast onto its queue. Main screen status auto-refresh not wired — user can re-push Session detail to see new state, or Sprint 5 event-stream lands true live-update.)*
- [x] `t` opens a read-only tail viewport showing recent log content. *(TailScreen reads the last 64 KiB from `<workspace>/logs/session.log` via filesystem read — same shape as the existing CLI's `mux sessions tail --follow=false`. No daemon endpoint needed; local-only assumption stated in ADR 0011's trust model.)*
- [x] `r` in the tail viewport re-fetches and updates the content. *(TailScreen binds `r` → `loadTailCmd(workspace)`.)*
- [x] Generic confirmation modal is reusable. *(Lives in `internal/tui/modal/` as a standalone package so Sprint 3 delete flows can import without touching tui internals.)*

#### Test plan

- Unit: confirmation-modal reducer test (y / N / Esc paths).
- Unit: `GetSessionLog` client test against `httptest.Server`.
- Manual: launch a session, press `x`, confirm termination; press `t` on an old (completed) session and confirm log renders.

#### Scope fences

- Do not implement force-kill in this task — `force=false` is enough for v0.0.3. A future task can add `X` or a palette verb for force stop.
- Do not implement log search inside the tail viewport.
- Do not wire tail-log to auto-follow — that's what the attach panel is for.
- Do not delete the session record after stop. Lifecycle transitions only.

#### Relationship

Depends on: T-v003-s02-01, T-v003-s02-02, T-v003-s02-04.
Reused by: Sprint 3 delete confirmations (reuse the modal).

#### Origin

User sketch: "Session stop (`x` with confirmation) and tail-log snapshot (`t`) from session detail." Epic v0.0.3 exit criteria: session management surface.

## Review / readiness notes

- **SSE vs WebSocket.** T-04 assumes SSE per v002-s05-02. If the v0.0.2 readiness review flipped to WebSocket, update the client wrapper choice. Cost: swap one library; the AttachEvent channel pattern stays.
- **Tail-log endpoint shape.** T-05 assumes either `GET /sessions/{id}/attach?snapshot=true` or a new `GET /sessions/{id}/log?limit=N`. Sprint v002-05 doesn't specify snapshot-mode explicitly. Capture as readiness gap — easy to add a small task to v002-05 or to implement against whatever the existing `mux sessions tail` CLI path uses (which already works per v002-s02 exit criteria).
- **Palette verb arguments.** T-03 deliberately ships no-argument verbs only. Sprints 3/4/5 will be tempted to add argument capture (`edit project <id>`). Defer unless a concrete need arrives; simple verbs that open a list-filter-then-select flow are fine for MVP.
- **ANSI handling inside attach.** Bubble Tea viewports don't natively re-render foreign ANSI screen apps (`vim`, `top`). Acceptable for v0.0.3 — the user can drop to a raw terminal for heavy interactive sessions. If ANSI fidelity becomes a complaint, v0.3+ can adopt `charmbracelet/x/ansi` or similar.
- **Refresh strategy.** Post-stop, the main screen doesn't live-update the session state until manual refresh or event-stream integration (Sprint 5). Consider adding a simple `r` refresh key in T-05 as a stopgap.
- **Input focus transitions.** When the palette overlay is up, main-screen search input must not capture keystrokes. The screen-stack's "top owns input" rule needs a clean test; flag if the first run reveals subtle routing bugs.
