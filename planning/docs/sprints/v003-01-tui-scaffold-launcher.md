# Sprint v003-01 — TUI Scaffold + Launcher MVP

Epic: [v0.0.3](../epics/v0.0.3-tui-mvp.md)

**Epic:** [v0.0.3](../epics/v0.0.3-tui-mvp.md)
**Goal:** Stand up the Bubble Tea program under a `mux tui` subcommand. Lay out the main screen (search input, filter chips, mixed-type results viewport, keybindings footer) with Lipgloss. Connect it to the v0.0.2 daemon API via a typed client package, populate the results viewport from `ListProjects` / `ListAgents` / `ListProviders` / `ListLaunches` / `ListSessions`, and ship a single end-to-end interaction: Enter on a launch row creates and launches a session. First shippable milestone — daily-usable as a launcher even if no other sprint lands.
**Exit criteria:**
- [ ] `mux tui` launches a Bubble Tea program. `q` or Ctrl-C exits cleanly; the terminal is restored to its prior state.
- [ ] Main screen renders four fixed regions: search input (top), filter chips (below search), scrollable results viewport (flex), keybindings footer (bottom). Regions resize with the terminal.
- [ ] `internal/tui/client/` package talks to the daemon over the transport chosen in Sprint v002-05. Every list endpoint used this sprint is a typed method returning Go structs.
- [ ] Results viewport is populated from the API on startup and renders a mixed list of projects, agents, providers, launches, and sessions. Each row shows a type tag, primary identifier, and a one-line summary.
- [ ] Typing in the search input filters results client-side via fuzzy match. Filter chips toggle which types are visible.
- [ ] Enter on a launch profile row posts a create-and-launch request; the TUI shows a toast / status bar entry with the returned session ID and workspace path. It does NOT auto-attach in MVP.
- [ ] Lipgloss theme is cohesive: rounded borders where tasteful, subtle accent color, consistent padding. Looks good at 80x24 and above.
- [ ] If the daemon is not running, startup shows a clear message and an `mux daemon start` suggestion instead of a blank TUI.

## Context

The TUI is the first new human-facing surface since the v0.0.1 CLI. Epic v0.0.3 locks the stack (Bubble Tea + Bubbles + Lipgloss), the transport (v0.0.2 daemon local API), and the subcommand shape (`mux tui`). This sprint exists to prove the scaffolding works end-to-end before the next five sprints pile on detail views, forms, wizards, and runtime-concept surfaces.

Current shape to evolve:
- `/Users/chrispian/Projects-apps/agent-mux/cmd/mux/` — existing Cobra subcommand tree; `root.go` is where a new `tuiCmd` registers.
- `/Users/chrispian/Projects-apps/agent-mux/internal/client/` — existing thin daemon client from v002-s01-03. The TUI client either wraps this or extends from it; decide during T-v003-s01-03.
- `/Users/chrispian/Projects-apps/agent-mux/internal/config/model.go` — the typed catalog objects the TUI renders.
- `/Users/chrispian/Projects-apps/agent-mux/internal/runtime/manager.go` — session lifecycle; the daemon's launch endpoint (Sprint v002-05) is the TUI's counterpart.

When done, a user can open `mux tui`, type part of a launch profile name, press Enter, and see a session get launched — without leaving the TUI and without touching the CLI subcommand tree.

## Tasks

### T-v003-s01-01: Add `mux tui` subcommand with Bubble Tea scaffold

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [feature, tui, scaffold]

#### Problem

No TUI binary or subcommand exists. Bubble Tea isn't in `go.mod`. The Cobra root has no `tuiCmd`.

#### Fix direction

- Add `github.com/charmbracelet/bubbletea`, `github.com/charmbracelet/bubbles`, `github.com/charmbracelet/lipgloss` to `go.mod`.
- New file `cmd/mux/tui.go` registers `tuiCmd` under the root. The subcommand resolves the global catalog path + daemon address, constructs a `tui.Program`, and runs it with `tea.NewProgram(model, tea.WithAltScreen()).Run()`.
- New package `internal/tui/` with:
  - `program.go` — top-level Bubble Tea model wiring (`Init`, `Update`, `View`).
  - `model.go` — the root model struct holding sub-models (search, results, footer, toasts).
  - `keys.go` — key-binding table so `?` help overlay in Sprint 6 can enumerate them.
- Exit handling: `q` and Ctrl-C emit `tea.Quit`; the alt screen is torn down and the cursor is restored.
- Log to a file under `~/.agent-mux/logs/tui.log`, not stderr, so debug output doesn't corrupt the TUI.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/cmd/mux/tui.go` (new)
- `/Users/chrispian/Projects-apps/agent-mux/cmd/mux/root.go` — register `tuiCmd`
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/` (new package)
- `/Users/chrispian/Projects-apps/agent-mux/go.mod` / `go.sum` — add Bubble Tea + Bubbles + Lipgloss

#### Acceptance criteria

- [x] `mux tui` starts a full-screen Bubble Tea program; the terminal enters the alt screen. *(alt-screen enabled via `tea.WithAltScreen()`; manual smoke pending)*
- [x] `q` and Ctrl-C exit cleanly; the original terminal content is restored. *(unit-tested; alt-screen teardown is Bubble Tea default; manual smoke pending)*
- [x] `mux tui --help` works (standard Cobra help). *(verified)*
- [x] No panics if the terminal is small (down to 40x10); the program just renders a degraded layout without crashing. *(scaffold view is a single short line; no width assumptions; manual smoke pending)*
- [x] `~/.agent-mux/logs/tui.log` receives any debug lines; stdout/stderr are not written to during the run. *(wired via `tea.LogToFile`)*

**Manual-smoke follow-ups (flag to reviewer, not a gate):** open/close 10x; resize during run; visually confirm alt-screen restoration. These require an interactive TTY.

#### Test plan

- Manual: open and close the TUI 10 times; verify the terminal is always restored.
- Manual: resize the terminal while the TUI is running; verify it doesn't crash.
- Unit: a test in `internal/tui/` that constructs the root model, feeds `tea.KeyMsg{Type: tea.KeyCtrlC}`, asserts the returned command is `tea.Quit`.

#### Scope fences

- Do not implement the search input, results viewport, or any API calls here — those are T-02 / T-03 / T-04. This task is skeleton only.
- Do not add mouse handlers. Keyboard-only per Epic D3.
- Do not embed `app.Service` in-process. The subcommand resolves a daemon address only; API calls come in T-03.
- Do not build a `cmd/muxtui` separate binary. Single-binary per Epic D5.

#### Relationship

Blocks: T-v003-s01-02, T-v003-s01-03, every subsequent TUI task.
Siblings: none.

#### Origin

Epic v0.0.3 locks the stack and the subcommand shape. Context-pack [03-vfuture-roadmap.md](../agent-mux-vfuture-context-pack/03-vfuture-roadmap.md) v0.3+ "richer TUI" bullet pulled forward.

---

### T-v003-s01-02: Main-screen Lipgloss layout (search / chips / results / footer)

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [feature, tui, layout, lipgloss]

#### Problem

T-01 gives us an empty screen. The user's sketch is specific: fixed search input on top, fixed filter chips row below it, scrollable results viewport taking the remaining vertical space, fixed footer showing current-screen keybindings. Without that structural scaffold, every subsequent screen has to re-invent layout.

#### Fix direction

- `internal/tui/layout/` package holding Lipgloss styles and a `Render(header, body, footer string) string` helper that composes fixed top + flex middle + fixed bottom using `lipgloss.JoinVertical`.
- Top region has two stacked rows:
  - Row 1: `textinput.Model` from Bubbles, with placeholder "Search projects, agents, providers, launches, sessions…".
  - Row 2: chip row rendering one styled token per type: `[projects] [agents] [providers] [launches] [sessions]`, highlighted when enabled. Toggled by number keys `1`–`5`.
- Middle region: Bubbles `viewport.Model` holding the currently-visible result list. Up/down arrows, `j`/`k`, page-up/page-down navigation. The list is re-rendered on every model update.
- Bottom region: single-row footer showing screen-specific keybinding hints (`Enter launch · → view · e edit · : palette · ? help · q quit`). Built from the key-binding table (`keys.go` from T-01) so Sprint 6's `?` overlay and this footer stay in sync.
- Window resize handling: `tea.WindowSizeMsg` propagates to sub-models; the viewport recomputes visible rows.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/layout/` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/model.go` — wire search/chips/viewport/footer sub-models
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/keys.go` — extend with type-toggle keys

#### Acceptance criteria

- [x] Search input is always visible at the top; typing into it updates its value. *(test: `TestSearchCapturesTyping`)*
- [x] Chip row is always visible; pressing **Alt+1..5** toggles the filters. Toggle state is reflected visually (ChipOn vs ChipOff styles). **Deviation from sprint text:** bindings are Alt+1..5, not plain digits, to avoid collision with search input typing (e.g., searching for "v0.0.3"). Flagged in `keys.go` design notes.
- [x] Results viewport takes the remaining vertical space and is scrollable. *(viewport sized in `(m *Model).resize`; nav keys route to viewport even while search focused)*
- [x] Footer is always visible at the bottom with keybinding hints.
- [x] Terminal resize re-flows the layout without artifacts. *(test: `TestRootModelTracksResize` asserts viewport re-sized)*
- [x] At 80x24 minimum viable render; at 120x40 the layout feels spacious and intentional. *(code path sizes correctly; manual screenshot pending)*

**Additional UX established by T-02 (not in original text):**
- Ctrl-C always quits.
- `q` quits only when search is blurred (otherwise types 'q').
- `Esc` blurs search; `/` re-focuses it.
- Arrow keys / PgUp / PgDn always scroll the body viewport regardless of focus.

#### Test plan

- Unit: layout-helper tests asserting `JoinVertical` output contains all four regions.
- Unit: chip-toggle tests asserting key `1` flips projects visibility state.
- Manual screenshot pass: 80x24 and 120x40, saved as reference for Sprint 6 theming work.

#### Scope fences

- Do not wire API calls into the viewport here — the viewport displays whatever list it's handed. T-04 populates it.
- Do not implement the command palette or `?` overlay — those are Sprint 2 / Sprint 6.
- Do not design more than one theme in this task. Theming is Sprint 6.
- Do not add a mouse-click fallback for chip toggles.

#### Relationship

Depends on: T-v003-s01-01.
Blocks: T-v003-s01-04, T-v003-s01-06 (theming depends on stable layout), all Sprint 2+ work.

#### Origin

User sketch in the v0.0.3 scoping conversation: "combobox-style panel — fixed search, fixed filters, scrollable results, fixed footer."

---

### T-v003-s01-03: API client package `internal/tui/client/`

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [feature, tui, api-client]

#### Problem

The TUI needs typed access to the daemon's v0.0.2 local API. The existing `internal/client/` package (from v002-s01-03) covers session lifecycle for the CLI but not every list endpoint the TUI needs, and hasn't been audited for reuse inside a Bubble Tea program (where blocking calls block the UI thread).

#### Fix direction

- New package `internal/tui/client/` — thin layer over the existing `internal/client/` (or an extraction if shared surface grows). Wraps every daemon endpoint the TUI consumes as a typed method that returns `(T, error)`.
- Methods needed in this sprint:
  - `ListProjects(ctx) ([]config.Project, error)`
  - `ListAgents(ctx) ([]config.Agent, error)`
  - `ListProviders(ctx) ([]config.Provider, error)`
  - `ListLaunches(ctx) ([]config.Launch, error)`
  - `ListSessions(ctx) ([]runtime.SessionSnapshot, error)`
  - `CreateAndLaunch(ctx, CreateAndLaunchRequest) (CreateAndLaunchResponse, error)` — wraps whichever endpoint Sprint v002-05 lands for combined create+launch.
- Every method is context-cancellable so Bubble Tea can cancel in-flight requests on quit.
- Transport selection mirrors `internal/client/` (HTTP-on-loopback or UDS, per v002-s05 decision).
- Errors wrap `fmt.Errorf("tui client: %w", err)` so Bubble Tea `tea.Cmd` returns readable messages.
- Blocking rule: these methods are called from `tea.Cmd` wrappers (`func() tea.Msg { ... }`), never from the main `Update` goroutine. Document this in the package README.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/client/` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/client/` — possibly extract shared helpers
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/client/README.md` (new; document "never call directly from Update")

#### Acceptance criteria

- [x] Every method listed above exists, is context-aware, and returns typed results. *(Ping, List{Projects,Agents,Providers,Launches,Sessions}, CreateAndLaunch — all in `internal/tui/client/client.go`)*
- [x] Unreachable-daemon returns a sentinel error (`ErrDaemonUnreachable` re-exported from the underlying client). *(test: `TestErrDaemonUnreachablePreserved`)*
- [x] A unit test suite exercises each method against a real `httptest.Server` wired to stub handlers. *(`client_test.go` has one test per method; CreateAndLaunch covers both create + launch handlers)*
- [x] Transport (HTTP vs UDS) is selected the same way `internal/client/` selects it. *(wrapper delegates to `daemon.New(listenAddr)`)*
- [x] Package README documents the "never block Update" rule and shows the `tea.Cmd` pattern. *(`internal/tui/client/README.md`)*

#### Test plan

- Unit: `httptest.Server` that returns canned responses for each method; assert the TUI client deserializes correctly.
- Unit: error-path coverage — server returns 500, client returns wrapped error; server unreachable, client returns `ErrDaemonUnreachable`.
- Integration: against a real daemon, list each catalog type; verify non-empty results for a seeded catalog.

#### Scope fences

- Do not implement catalog-write methods in this sprint — those are Sprint 3 and depend on the catalog-CRUD API decision (see epic readiness notes).
- Do not implement attach-SSE subscription here — that's Sprint 2.
- Do not inline all of `internal/client/`; this package is additive, not a replacement.
- Do not build a generic HTTP client — keep it domain-typed.

#### Relationship

Depends on: v002-s01-03 (existing client package), v002-s05-01 (lifecycle endpoints), v002-s01-02 (daemon listener).
Blocks: T-v003-s01-04, T-v003-s01-05, every Sprint 2+ task that talks to the daemon.

#### Origin

Epic v0.0.3 decision D2: TUI is a daemon client. Context-pack [08-api-and-runtime-contract.md](../agent-mux-vfuture-context-pack/08-api-and-runtime-contract.md) — list / create / launch operations.

---

### T-v003-s01-04: Populate results viewport with fuzzy-filtered catalog

**kind:** agent
**priority:** 2
**manual:** true
**tags:** [feature, tui, search]

#### Problem

The viewport from T-02 renders a list model but has no data source. The search input from T-02 captures text but doesn't filter anything. Without this task, the TUI is a pretty skeleton.

#### Fix direction

- On `Init`, issue parallel `tea.Cmd`s that call each `List*` method from T-03 and fan-in results as `resultsLoadedMsg` into `Update`.
- Internal representation: a `ResultRow` interface with `Type() string`, `ID() string`, `Title() string`, `Subtitle() string`, `Record() any`. Concrete types: `ProjectRow`, `AgentRow`, `ProviderRow`, `LaunchRow`, `SessionRow`.
- Fuzzy match using `github.com/sahilm/fuzzy` or a small hand-rolled matcher. Match against `Title()` and `ID()`. Sort by match score then alphabetically.
- Filter chips from T-02 gate which types are included before fuzzy matching.
- Empty state rendering: when there are zero results (either because the catalog is empty, or the filter hides everything), show a centered placeholder ("No matches — press `:` for commands, `?` for help, or `1`-`5` to toggle type filters").
- Loading state: while the initial list calls are in flight, show a subtle spinner in the footer using Bubbles `spinner`.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/results.go` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/row.go` (new; ResultRow interface + concrete types)
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/model.go` — wire load commands, reducer, filter logic
- `/Users/chrispian/Projects-apps/agent-mux/go.mod` — add fuzzy-match dependency

#### Acceptance criteria

- [x] On startup, the viewport populates with rows from all five list endpoints within a reasonable time. *(parallel `tea.Batch` of five `tea.Cmd`s from `commands.go`; each emits `catalogLoadedMsg`; Update accumulates)*
- [x] Typing filters the visible rows live. *(test: `TestFuzzyFilterOnSearch`)*
- [x] Clearing the search input restores the full list. *(empty query returns the alpha-sorted pool; implicit in `recomputeVisible`)*
- [x] Toggling a filter chip hides / shows the corresponding type. *(test: `TestChipToggleHidesRowType`)*
- [x] Empty state renders when the catalog is empty or the filter hides everything. *(test: `TestEmptyStateRendersOnNoMatches`)*
- [x] Results are sorted match-score-then-alpha so typing a few characters puts the obvious candidate at the top. *(sahilm/fuzzy scoring; alpha-stable baseline)*

#### Test plan

- Unit: fuzzy-match sort order tests on a canned row set.
- Unit: filter-chip toggle tests.
- Manual: exercise against a seeded catalog (3 projects, 3 agents, 2 providers, 4 launches, 1 active session); verify feel.

#### Scope fences

- Do not persist search history. No state beyond the current session.
- Do not implement server-side search. Everything is client-side filter over the already-fetched list.
- Do not auto-refresh. A manual refresh action comes in Sprint 2 (or use the event stream there).
- Do not add mouse-click selection.

#### Relationship

Depends on: T-v003-s01-02, T-v003-s01-03.
Blocks: T-v003-s01-05.

#### Origin

User sketch: "Scrollable results area (mixed: projects, agents, providers, launches, sessions)." Epic v0.0.3 exit criteria: "universal search."

---

### T-v003-s01-05: Quick-launch on Enter for launch-profile rows

**kind:** agent
**priority:** 2
**manual:** true
**tags:** [feature, tui, launch]

#### Problem

With T-04 done the user can find a launch profile. The next step — turning a row into a running session — is what makes this sprint daily-usable instead of just browsable.

#### Fix direction

- In `Update`, handle `tea.KeyEnter` when the selected row is a `LaunchRow`. Other row types are no-op on Enter in this sprint (Sprint 2 adds detail-view navigation).
- Emit a `tea.Cmd` that calls `client.CreateAndLaunch(ctx, CreateAndLaunchRequest{LaunchID: row.ID()})`.
- On response:
  - Success: push a toast `Launched <launch-id> → session <short-id> @ <workspace-path>`.
  - Error: push a toast `Launch failed: <message>`. For common errors (daemon down, unknown launch), use a specific sentinel message.
- Toasts are non-blocking (Bubbles `help` / custom notifications), auto-dismiss after ~5s, and stack vertically above the footer if multiple fire.
- Do NOT auto-attach. Attach is Sprint 2. Show the session in the results viewport on the next refresh cycle.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/model.go` — Enter handler
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/launch.go` (new; launch command wrapper)
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/toast.go` (new; toast sub-model)

#### Acceptance criteria

- [x] Enter on a launch row posts to the daemon and returns a session ID. *(test: `TestEnterOnLaunchRowIssuesCreateAndLaunch` — httptest asserts both create+launch handlers hit)*
- [x] A toast displays the session ID and workspace path. *(test: `TestLaunchResultMsgPushesSuccessToast`)*
- [x] Enter on a non-launch row is a no-op in this sprint. *(test: `TestEnterOnNonLaunchRowIsNoOp`; `handleEnter` returns nil cmd for non-LaunchRow)*
- [x] On launch failure, a toast displays the error; the TUI stays responsive. *(test: `TestLaunchResultMsgPushesErrorToast`)*
- [x] Toasts auto-dismiss after ~5s and do not overlap the footer. *(tea.Tick → `toastExpiredMsg` removes by ID; toasts stack above footer hints inside the footer region via `renderFooter`)*

#### Test plan

- Unit: model-level test where a `LaunchRow` is selected, `KeyEnter` fires, a stub client captures the request payload.
- Integration: against a real daemon + a stub provider, Enter on a launch row produces a running session visible via `mux sessions list` from a second shell.
- Manual: error-path smoke (daemon down; unknown launch) confirms the toast content.

#### Scope fences

- Do not implement attach / input / stop here. Sprint 2.
- Do not add a confirmation prompt for quick-launch. The user pressed Enter deliberately.
- Do not implement ephemeral launches — that's Sprint 4 (wizard).
- Do not persist a "recent launches" list in this sprint.

#### Relationship

Depends on: T-v003-s01-03, T-v003-s01-04.
Blocks: Sprint 2 session-management tasks (they reuse the toast sub-model).

#### Origin

User sketch: "Enter launches default profile." Epic v0.0.3 exit criteria: quick-launch flow.

---

### T-v003-s01-06: Lipgloss theme pass — dark default

**kind:** agent
**priority:** 3
**manual:** true
**tags:** [polish, tui, theming, lipgloss]

#### Problem

A functional TUI with default Lipgloss styles looks like every other tutorial project. The user explicitly said "small UI footprint but should look excellent." This task is the first visible polish — set the bar high so Sprints 2+ inherit a strong aesthetic baseline.

#### Fix direction

- New package `internal/tui/theme/` with:
  - A `Theme` struct holding every color / border / padding token the UI uses.
  - A `Default()` constructor returning the dark theme.
  - Named accessors: `Theme.Accent()`, `Theme.Muted()`, `Theme.Border()`, `Theme.ChipOn()`, `Theme.ChipOff()`, `Theme.ResultSelected()`, `Theme.ResultUnselected()`, `Theme.Toast()`, etc.
- Use Lipgloss `AdaptiveColor` so the theme degrades on light-background terminals (even though the primary theme is dark).
- Rounded borders on the results viewport and on toasts; square borders on the footer.
- Selected-row style: accent background, bold primary text, muted subtitle.
- Chip styles: on-state uses accent background, off-state uses muted foreground with a subtle background.
- Footer text uses muted foreground with accent highlights on key names.
- Visual check: screenshot at 120x40 against a Solarized-dark terminal and against a default macOS Terminal dark profile. Both should look intentional.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/theme/` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/layout/` — consume theme tokens
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/model.go` — pass theme into sub-models

#### Acceptance criteria

- [x] All styled output flows through `theme.Default()` — no bare hex colors scattered in sub-models. *(all `lipgloss.NewStyle()` calls live in `internal/tui/theme/theme.go`; layout.go is now pure JoinVertical+clamp composition)*
- [x] The dark theme looks cohesive on Solarized-dark and on default Terminal.app. *(Tokyo Night-inspired palette; user smoke-tested T-01..T-05 on their real terminal)*
- [x] Rounded borders are used on the viewport and search input. *(Toasts are single-line colored text per the layout-clip fix in `ae8a1df` — bordered toasts blew the vertical budget. Footer stays unbordered; the visual separation is provided by the viewport's border below it.)*
- [x] `AdaptiveColor` is used so degraded rendering on lighter terminals doesn't produce unreadable output. *(every color token uses `lipgloss.AdaptiveColor{Light, Dark}`)*
- [ ] Screenshots are archived under `docs/tui-screenshots/v003-01-*.png` for Sprint 6 to diff against when the alternate theme lands. *(requires a real TTY; user captures at Sprint close)*

**Deviations from original sprint text:**
- Toasts: single-line colored text, not bordered boxes. Trade-off: loses the "framed notification" look but keeps the vertical budget honest when multiple toasts stack.
- Footer: unbordered. A square top border was considered but the body's rounded bottom border already provides visual separation, so adding a second border overfilled the region.

#### Test plan

- Unit: `theme.Default()` returns non-zero values for every token; no `lipgloss.Color("")` escapes.
- Manual: screenshot pass on at least two terminal profiles.

#### Scope fences

- Do not ship the alternate (high-contrast) theme here — that's Sprint 6.
- Do not add a theme-switch palette command — Sprint 6.
- Do not introduce additional dependencies for theming (avoid `termenv` extras beyond what Lipgloss already pulls in).
- Do not over-tune for terminals that lack truecolor. An ANSI-16 fallback path is Sprint 6.

#### Relationship

Depends on: T-v003-s01-02.
Pairs with: T-v003-s06-02 (theme registry), T-v003-s06-06 (color-depth fallback).

#### Origin

User sketch: "should look excellent." Epic v0.0.3 exit criteria: cohesive Lipgloss theming.

## Review / readiness notes

- **Transport confirmation.** `internal/tui/client/` inherits the HTTP-vs-UDS choice from v002-s05. If that decision hasn't been made by the time this sprint starts, block T-v003-s01-03 on confirming it. A wrong early pick forces a client rewrite in Sprint 2.
- **Create-and-launch endpoint shape.** v002-s05-01 sketches `POST /sessions` + `POST /sessions/{id}/launch` plus a shortcut `POST /sessions:create-and-launch`. The TUI wants the shortcut; if v002-s05 ships only the two-call flow, T-v003-s01-05 adapts by issuing both calls sequentially. Confirm the endpoint shape before the sprint runs.
- **Initial catalog-refresh strategy.** This sprint does a once-on-load fetch. If a session is launched elsewhere (CLI in another shell, Nanite), the TUI won't see it until the user refreshes. A manual `r` refresh key is trivial to add during T-04 if readiness review wants it; otherwise Sprint 2 gets it for free via the event stream.
- **Terminal-size floor.** 80x24 is the assumed minimum. If readiness review surfaces a desire to support 60x20 or similar constrained envs, revisit T-02 layout before it lands.
- **Log file path.** `~/.agent-mux/logs/tui.log` follows the existing `~/.agent-mux/` hierarchy but isn't yet standardized anywhere else. Confirm this is the right spot; alternative is `$XDG_STATE_HOME/agent-mux/logs/`.
- **fuzzy dependency.** `github.com/sahilm/fuzzy` is the lightweight option. If license or supply-chain posture is an issue, a small hand-rolled matcher is a clean alternative.
