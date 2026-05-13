# Sprint v003-06 — Polish, Help, Theming, Discoverability

Epic: [v0.0.3](../epics/v0.0.3-tui-mvp.md)

**Epic:** [v0.0.3](../epics/v0.0.3-tui-mvp.md)
**Goal:** The ship-to-users quality pass. Add a `?` help overlay that enumerates the current screen's keybindings. Ship a second named theme and make theme selection switchable at runtime. Add first-run onboarding so an empty catalog doesn't feel broken. Add toasts / notifications with stacking + auto-dismiss. Audit keybindings for consistency and publish a reference doc. Document minimum terminal size and color-depth fallback behavior.
**Exit criteria:**
- [ ] `?` overlay is reachable from every screen; enumerates the current screen's keybindings grouped sensibly.
- [ ] At least two named themes ship: a dark default and one alternate (high-contrast). Palette verb `theme <name>` switches themes at runtime without restarting.
- [ ] First-run onboarding: if the catalog is empty on startup, show a hint card pointing at `mux projects add` or the wizard (`w`).
- [ ] Toasts / notifications exist with stacking + auto-dismiss; used by every action that previously used ad-hoc status-bar writes.
- [ ] `docs/tui-keybindings.md` enumerates every binding grouped by screen. An audit task reconciles collisions and inconsistencies surfaced during Sprints 1–5.
- [ ] `docs/tui-keybindings.md` also documents minimum terminal size and the ANSI-16 color-depth fallback path.

## Context

Sprints 1–5 ship features; this one ships polish. The explicit goal is "make it feel like a tool you enjoy using." Every task here is a second pass over something earlier sprints landed, not a new surface.

Current shape to evolve:
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/keys.go` (from Sprint 1) — already tracks keybindings per screen; Sprint 6 makes this a first-class source of truth.
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/theme/` (from Sprint 1) — single theme; Sprint 6 adds registry + switching.
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/toast.go` (from Sprint 1) — basic implementation; Sprint 6 refines stacking and reuses it everywhere.
- `/Users/chrispian/Projects-apps/agent-mux-v0-pack/docs/` — the docs home for `tui-keybindings.md`.

When done, v0.0.3 is ready to put in front of users.

## Tasks

### T-v003-s06-01: `?` help overlay per screen

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [feature, tui, help, discoverability]

#### Problem

The footer shows a handful of keybindings. The real binding set per screen is larger (wizard exits, modal controls, form-field navigation). Users need a one-key reference they can pull up without leaving their current context.

#### Fix direction

- New screen-overlay `internal/tui/help/overlay.go` implementing `Screen`:
  - Trigger: `?` at any screen. Pushes the overlay onto the stack; Esc pops.
  - Content: enumerates `stack.Top().KeyBindings()` (the `Screen` interface method from Sprint 2) grouped by category. Categories are declared by each screen via a `[]KeyBindingGroup` return type.
  - Layout: two-column centered modal with a bordered Lipgloss panel. Long binding lists paginate; `j`/`k` / arrow-keys scroll.
  - Header: "Keybindings — {screen.Title()}". Footer: "Esc to close".
- Every screen extended in Sprints 1–5 must provide a proper `KeyBindings()` grouping (search/navigation, actions, modal, palette). This task audits and fills in any gaps.
- `docs/tui-keybindings.md` (T-05) renders the same grouping — one source of truth.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/help/` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/screen/` — extend `Screen` interface to return `[]KeyBindingGroup`
- Every package under `internal/tui/detail/`, `internal/tui/crud/`, `internal/tui/wizard/`, `internal/tui/runtime/` — add grouped keybinding declarations

#### Acceptance criteria

- [ ] `?` from every screen opens the overlay.
- [ ] Overlay shows the current screen's bindings grouped.
- [ ] Esc closes the overlay and returns to the underlying screen.
- [ ] A lint-style test verifies every `Screen` implementation returns at least one non-empty `KeyBindingGroup`.
- [ ] Overlay is readable on 80x24.

#### Test plan

- Unit: overlay push / pop test.
- Unit: per-screen `KeyBindings()` coverage test.
- Manual: open `?` from main, detail views, wizard, attach panel, palette — confirm each renders correctly.

#### Scope fences

- Do not embed full prose docs in the overlay. Short binding descriptions only; the ref doc is T-05.
- Do not implement context-aware filtering ("show only bindings that apply right now"). All bindings for the current screen is fine.
- Do not add "cheat sheet" printable export.
- Do not support search within the overlay.

#### Relationship

Depends on: T-v003-s02-01 (Screen interface), every prior sprint's `Screen` implementations.
Pairs with: T-v003-s06-05 (doc derived from the same source of truth).

#### Origin

Epic v0.0.3 exit criteria: `?` help overlay. User sketch: "`?` help overlay exists."

---

### T-v003-s06-02: Theme registry + runtime switching

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [feature, tui, theming, polish]

#### Problem

Sprint 1 ships a single `Default()` dark theme. Epic v0.0.3 D4 commits to a second theme (high-contrast) and a palette-driven switch. Without a registry, later theme additions require touching every subpackage.

#### Fix direction

- Extend `internal/tui/theme/`:
  - `Registry` struct with `Register(name string, factory func() *Theme)` and `Get(name string) *Theme`.
  - Built-in registrations: `dark` (rename `Default` accessor, keep backwards compat), `high-contrast`.
  - High-contrast theme: true-black background, true-white foreground, bold accent (bright yellow or bright cyan), no subtle muting. Validated against WCAG AA where practical for terminals.
- Add a "current theme" pointer to the root model; every sub-model receives `*Theme` by reference, not by value copy, so a switch triggers immediate re-render.
- Palette verb `theme dark` / `theme high-contrast` — set current, emit a `tea.Msg` that sub-models react to.
- Persist the last-used theme to `~/.agent-mux/tui/state.json` so the next launch uses the same theme. Read at startup; ignore if the file is missing or malformed.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/theme/registry.go` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/theme/highcontrast.go` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/model.go` — wire current-theme and switch message
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/state/` (new; persistent state file)

#### Acceptance criteria

- [ ] Both themes are registered and retrievable by name.
- [ ] Palette verb `theme <name>` swaps the current theme at runtime without re-rendering glitches.
- [ ] Unknown theme name surfaces a toast `No theme named <name>`.
- [ ] Last-used theme is persisted and restored on next launch.
- [ ] High-contrast theme remains coherent on all five main screens + attach panel + palette + help overlay.

#### Test plan

- Unit: registry round-trip.
- Unit: `theme` verb handler test.
- Manual: switch themes mid-session; verify no visual artifacts.
- Manual: quit and relaunch; verify the last theme is restored.

#### Scope fences

- Do not build a visual theme editor.
- Do not support loading themes from external TOML / YAML in v0.0.3 — in-code only.
- Do not support per-screen theme overrides.
- Do not make the persisted state file a general config home — it's theme-state-only for now.

#### Relationship

Depends on: T-v003-s01-06 (base theme package).
Pairs with: T-v003-s06-06 (color-depth fallback).

#### Origin

Epic v0.0.3 D4: "One alternate theme. Both ship in Sprint 6." User sketch: "switchable via palette."

---

### T-v003-s06-03: First-run onboarding hint card

**kind:** agent
**priority:** 2
**manual:** true
**tags:** [feature, tui, onboarding, discoverability]

#### Problem

If a user runs `mux tui` before adding any projects / agents / launches, the main screen shows an empty list and a blinking search box. No signal pointing at what to do next.

#### Fix direction

- New overlay `internal/tui/onboarding/hint.go`:
  - Triggered when all list calls return empty.
  - Content: bordered centered card with a short welcome message and three hints:
    - "Press `w` to open the launch wizard."
    - "Press `:` then type `add project` to create a project."
    - "Read `~/.agent-mux/catalog/README.md` (or docs) for catalog structure."
  - Dismissed with any key. Once dismissed, a state flag in `~/.agent-mux/tui/state.json` (shared with T-02) suppresses the card on future empty-catalog launches.
- The card does not block startup — the TUI is still interactive underneath; the card is a semi-modal hint.
- A palette verb `show onboarding` re-opens the card manually.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/onboarding/` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/state/` — extend with `onboarding_seen` flag
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/wire.go` — register `show onboarding` verb

#### Acceptance criteria

- [ ] Empty catalog triggers the onboarding card on first run.
- [ ] Any key dismisses it; `onboarding_seen` flag is persisted.
- [ ] Second launch with an empty catalog does NOT show the card (flag is honored).
- [ ] Palette verb `show onboarding` re-opens it manually.
- [ ] Card is readable on 80x24.

#### Test plan

- Unit: empty-catalog detection test.
- Unit: state flag read / write.
- Manual: wipe state file, run, confirm card; dismiss; run again, confirm suppression; run palette verb, confirm re-open.

#### Scope fences

- Do not make this a multi-step onboarding flow. One card is enough.
- Do not link to external URLs.
- Do not auto-open the wizard from the card — the user still presses `w` or the palette verb.
- Do not show the card when only one category is empty (e.g. has projects but no launches).

#### Relationship

Depends on: T-v003-s06-02 (state file plumbing).
Pairs with: T-v003-s01-05 (toast pattern extended here).

#### Origin

Epic v0.0.3 exit criteria: first-run onboarding.

---

### T-v003-s06-04: Toasts — stacking, auto-dismiss, audit

**kind:** agent
**priority:** 2
**manual:** true
**tags:** [polish, tui, toasts, notifications]

#### Problem

Sprint 1 shipped a basic toast sub-model. By Sprint 5 it's used by launch, stop, save, delete, validation errors, event-stream errors — and stacking / positioning behavior has likely drifted. Audit and standardize.

#### Fix direction

- Extend `internal/tui/toast.go`:
  - Toast severities: `info`, `success`, `warning`, `error`. Each renders with a distinct theme accent.
  - Stacking: multiple toasts stack vertically above the footer, newest on top. Capped at 5 visible simultaneously; older toasts evict on new arrivals.
  - Auto-dismiss: `info` and `success` after 5s; `warning` after 10s; `error` sticky until dismissed.
  - Manual dismiss: `Ctrl-D` clears all toasts.
- Audit Sprint 1–5 callers; replace ad-hoc status-bar writes with `toast.Push(ctx, severity, message)`.
- Add a single-line status bar alongside toasts for stable state (daemon address, session count, current theme).

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/toast.go`
- All packages under `internal/tui/` that emit status messages

#### Acceptance criteria

- [ ] Toast severities render distinctly.
- [ ] Up to 5 toasts stack; newer toasts evict older ones.
- [ ] Auto-dismiss timers respect severity.
- [ ] Ctrl-D clears all toasts.
- [ ] No `internal/tui/` package emits status text outside the toast system.

#### Test plan

- Unit: severity-timing tests (use a fake clock).
- Unit: stack-eviction test.
- Unit: Ctrl-D handler test.
- Manual: trigger rapid toast bursts (e.g. 10 back-to-back saves) and verify no UI corruption.

#### Scope fences

- Do not add toast queueing beyond the 5-visible cap.
- Do not add sound / bell notifications.
- Do not persist toasts across TUI restarts.
- Do not add toast action buttons (e.g. "Undo"). Fire-and-forget only.

#### Relationship

Depends on: T-v003-s01-05.
Consumers: every prior sprint.

#### Origin

Epic v0.0.3 exit criteria: toasts / notifications. User sketch: "non-blocking status messages (launch success, validation error, save). Auto-dismiss, stackable."

---

### T-v003-s06-05: Keybinding audit + `docs/tui-keybindings.md`

**kind:** agent
**priority:** 2
**manual:** true
**tags:** [polish, tui, docs, keybindings]

#### Problem

Sprints 1–5 introduced dozens of keybindings across screens. Collisions and inconsistencies (e.g. `e` for edit on a list, `E` for events overlay, `c` for both "checkpoint" and possibly elsewhere) accumulate. A deliberate audit now prevents user frustration later.

#### Fix direction

- Enumerate every binding by walking `Screen.KeyBindings()` across every screen package.
- Check for collisions within a single screen (same key, two bindings).
- Check for convention drift across screens:
  - `a` = add new on list views (canonical)
  - `e` = edit (canonical)
  - `d` = delete (canonical)
  - `s` = save inside form (canonical)
  - `:` / Ctrl-K = palette (canonical)
  - `?` = help (canonical)
  - `q` / Ctrl-C = quit (canonical)
  - Esc = cancel / back (canonical)
- Where drift is found, adjust screens to the canonical set; surface any decisions that can't be resolved by convention as readiness notes.
- Write `docs/tui-keybindings.md`:
  - One section per screen category.
  - Keybinding table with key, action, note.
  - Top section on global bindings (`q`, `:`, `?`, Esc, Ctrl-C, Ctrl-D, Ctrl-K).
  - Cross-ref the `?` overlay and the canonical conventions.
- The doc is kebab-case, matches existing doc naming conventions under `docs/`.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux-v0-pack/docs/tui-keybindings.md` (new)
- Screen packages — adjust bindings where drift is found

#### Acceptance criteria

- [ ] `docs/tui-keybindings.md` enumerates every binding grouped by screen.
- [ ] No collisions within any single screen.
- [ ] Canonical verbs (`a`/`e`/`d`/`s`/`:`/ `?`/`q`/Esc) are used consistently.
- [ ] The doc links from the v0.0.3 epic file.
- [ ] A lint-style test asserts no two bindings within a single screen claim the same key.

#### Test plan

- Unit: binding-uniqueness audit across all registered screens.
- Manual: skim the doc against the running TUI on each screen.

#### Scope fences

- Do not introduce a user-configurable keybinding system (no `~/.agent-mux/tui/keys.json`). v0.0.3 is fixed bindings.
- Do not introduce vim-style leader-key chords.
- Do not document external-tool key bindings (vim inside an attach panel, etc.).
- Do not re-bind palette / help / quit keys — those are locked.

#### Relationship

Depends on: every prior sprint landing its bindings.
Pairs with: T-v003-s06-01.

#### Origin

User sketch: "Keybinding consistency audit + `docs/tui-keybindings.md` reference doc (matching the kebab-case convention)."

---

### T-v003-s06-06: Accessibility / terminal-compat notes

**kind:** agent
**priority:** 3
**manual:** true
**tags:** [polish, tui, accessibility, compat]

#### Problem

The TUI assumes truecolor-capable terminals at 80x24 or larger. Users on ANSI-16 terminals, tmux with reduced color depth, or smaller windows need documented expectations — and ideally a graceful degraded render.

#### Fix direction

- Detect terminal color depth via Lipgloss / termenv at startup. Persist the detected profile in state for debugging.
- Add an ANSI-16 degrade path in `internal/tui/theme/`:
  - Each theme exposes an `ANSI16()` variant that maps accent colors to the 16-color palette.
  - When the detected depth is 16-color, switch themes automatically (bypass the dark/high-contrast choice). Log to the toast system: `Detected 16-color terminal — using ANSI-16 fallback theme.`
- Minimum-size handling:
  - Below 60x15, render a single centered "Terminal too small — resize to at least 80x24" message.
  - Between 60x15 and 80x24, render compact layout: hide chips, shorter footer, no status bar.
- Document in `docs/tui-keybindings.md` (T-05):
  - Minimum / recommended terminal size.
  - Color-depth behavior.
  - Known terminal compatibility list (iTerm2, Alacritty, Kitty, Terminal.app, tmux, screen, Windows Terminal).

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/theme/` — ANSI16 variants
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/layout/` — compact-mode rendering
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/model.go` — terminal-depth detection at startup
- `/Users/chrispian/Projects-apps/agent-mux-v0-pack/docs/tui-keybindings.md` — append compat section

#### Acceptance criteria

- [ ] ANSI-16 terminals auto-switch to the fallback theme without visual breakage.
- [ ] Terminals below the hard minimum render the "too small" message cleanly.
- [ ] Compact layout between the hard minimum and recommended size keeps the TUI usable.
- [ ] Compat section in the keybindings doc covers color-depth behavior, minimum size, and the terminal compatibility list.

#### Test plan

- Manual: launch under `TERM=xterm` and verify fallback theme.
- Manual: shrink the terminal below minimum and verify the graceful error.
- Manual: shrink into the compact window and confirm chips/footer hide predictably.

#### Scope fences

- Do not support 8-color (`TERM=vt100`). If detection yields 8-color, fall back to the 16-color path.
- Do not implement screen-reader integration. Terminals aren't a rich a11y surface; document "if you need a screen reader, use the CLI."
- Do not add bold-only fallback for no-color terminals beyond the ANSI-16 path.
- Do not add a config knob for minimum size — keep it fixed.

#### Relationship

Depends on: T-v003-s06-02 (theme registry), T-v003-s01-02 (layout).
Pairs with: T-v003-s06-05 (doc).

#### Origin

User sketch: "Accessibility / terminal-compat notes: document minimum terminal size, color-depth fallback (ANSI-16 path for TERM=xterm), keybinding collisions documented."

## Review / readiness notes

- **Persistent state file shape.** T-02 and T-03 both write to `~/.agent-mux/tui/state.json`. Agree on the schema early — `{theme: "dark", onboarding_seen: true}` is enough for v0.0.3 but allow room for forward-compat keys. Consider an ADR if the state file's reach grows.
- **Theme quality gate.** The high-contrast theme is not a trivial token swap — validate it on a separate session before declaring Sprint 6 done. Low-effort QA: screenshot the five main screens + attach panel under each theme and eyeball.
- **Keybinding collision discovery.** If T-05's audit surfaces more than a handful of collisions that need resolution, flag for a readiness conversation; some may need user-visible changes that warrant release notes.
- **Onboarding dismissal edge case.** If the catalog starts empty, gets populated (user adds a project), then gets emptied again (user deletes the project), should the onboarding card re-show? v0.0.3 answer: no (flag is sticky). v0.0.5+ could reset the flag after a grace period.
- **Color-depth auto-switch vs user intent.** If a user explicitly chose `high-contrast` but their terminal is 16-color, T-06 overrides to ANSI-16 fallback. Document this precedence clearly in the docs (T-05).
- **Sprint is polish-only.** Resist scope creep during the audit — if new polish ideas emerge (saved searches, per-screen theme overrides, user-configurable bindings), capture as v0.0.5+ backlog rather than expanding this sprint.
