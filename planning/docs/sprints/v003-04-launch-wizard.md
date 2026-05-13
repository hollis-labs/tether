# Sprint v003-04 — Launch Wizard

Epic: [v0.0.3](../epics/v0.0.3-tui-mvp.md)

**Epic:** [v0.0.3](../epics/v0.0.3-tui-mvp.md)
**Goal:** Add the multi-step guided launch flow the user sketched. A new session is composed by stepping through project → agent → provider → options → review, with three exits at the end: launch ephemerally (no YAML saved), save the composed plan as a launch YAML and launch it, or save-only. Reuses the form framework from Sprint 3 so field widgets stay consistent.
**Exit criteria:**
- [ ] A `launch wizard` palette verb (and a dedicated keybinding on the main screen, e.g. `w`) opens a five-step wizard.
- [ ] Each step renders a consistent header with a step indicator (e.g. `Step 2 / 5 — Pick Agent`), a body, and a footer showing `← back · → next · Esc cancel · : palette · ? help`.
- [ ] Step 1 picks a project; Step 2 picks an agent; Step 3 picks a provider; Step 4 captures options / tools / permissions / env overrides; Step 5 is a review with three exits: `l` launch ephemeral, `s` save-and-launch, `S` save-only.
- [ ] Ephemeral launch succeeds without writing any catalog YAML.
- [ ] Save-and-launch writes a new launch YAML and launches it.
- [ ] Save-only writes the YAML and pops back to the main screen with a toast.
- [ ] Esc / Ctrl-C at any step prompts for cancel-confirmation if the wizard is non-empty; if the user confirms, the wizard is discarded (state is NOT persisted across restarts; see epic readiness notes).

## Context

Sprint 3 gave the user the ability to author a launch YAML through a single-screen form. The wizard is a different UX: instead of "fill out a launch record," it's "compose a launch interactively, with the reassurance that you can try it once without committing to save it." Ephemeral launches are a first-class exit per Epic v0.0.3 decision D6.

Current shape to evolve:
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/form/` (from Sprint 3) — reused for Step 4 (options form) and for all select widgets.
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/screen/` (from Sprint 2) — the wizard is a screen stack internal to itself.
- Daemon API: needs to accept an inline-plan payload on `POST /sessions` (or `POST /sessions:create-and-launch`) for the ephemeral path. See epic readiness notes; flag as gap if not present.

When done, a user can compose and run a launch in under 30 seconds without touching any file on disk, and can promote that composition to a saved YAML at the exact same review step.

## Tasks

### T-v003-s04-01: Multi-step wizard framework

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [feature, tui, wizard, framework]

#### Problem

The wizard's structure (linear steps, forward/back nav, a review step) is reusable but distinct from the single-screen form framework. Without a shared framework, each step would reinvent navigation and the review-step exit handling.

#### Fix direction

- New package `internal/tui/wizard/`:
  - `Step` interface — `Init() tea.Cmd`, `Update(tea.Msg) (Step, tea.Cmd)`, `View() string`, `Title() string`, `Valid() bool` (can we advance?), `Summary() string` (text for the review step).
  - `Wizard` struct — holds `[]Step`, current-index pointer, shared state map (`map[string]any`) so earlier steps' choices are available to later ones.
  - Navigation keys: Right arrow / `n` advances if `Valid()`; Left arrow / `b` retreats; Esc cancels (prompts for confirmation if any step has any state).
  - Step indicator renders in the header: `Step {current+1} / {total} — {currentStep.Title()}`.
  - Implements the `Screen` interface (from Sprint 2's screen-stack), so it pushes onto the stack like anything else.
  - `State` helpers: `Wizard.Get(key string) any`, `Wizard.Set(key string, value any)` — used by steps to share picks.
- Review step is a special `ReviewStep` type that renders `Summary()` of every prior step and handles the three exit keys (`l`, `s`, `S`).

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/wizard/` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/wizard/README.md` (new; step-authoring pattern)

#### Acceptance criteria

- [ ] `Wizard` implements `Screen` and can be pushed onto the main stack.
- [ ] Step indicator renders current-step number out of total.
- [ ] Forward nav is blocked if `currentStep.Valid()` returns false; a toast explains why.
- [ ] Back nav is always possible until Step 1.
- [ ] Esc triggers a confirmation modal if any step has captured state; `y` cancels the wizard, `N` returns to the current step.
- [ ] Shared state map survives step-to-step navigation.

#### Test plan

- Unit: wizard-nav tests — build a 3-step dummy wizard, assert forward/back transitions and cancel-confirmation.
- Unit: `Wizard.Get/Set` round-trip test.
- Manual: run the wizard against stub steps and verify the header/step indicator visuals.

#### Scope fences

- Do not persist wizard state to disk in this task — cross-restart persistence is deferred per epic readiness.
- Do not implement branching (step N+1 depends on choice in N) — linear advance only.
- Do not skip validation on forward nav; an explicit `Valid()` must be passed.
- Do not allow arbitrary jumping between steps (no clickable step indicator).

#### Relationship

Depends on: T-v003-s02-01 (screen-stack), T-v003-s03-01 (form framework — steps use form fields).
Blocks: T-v003-s04-02, T-v003-s04-03, T-v003-s04-04.

#### Origin

User sketch: "Multi-step wizard framework: step model, next/back keys, step indicator in the header, review step at the end."

---

### T-v003-s04-02: Steps 1–3 — Project / Agent / Provider pickers

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [feature, tui, wizard, pickers]

#### Problem

The first three steps are all "pick one from a list, or add new inline." They share enough UX to warrant a shared picker, but each binds to a different list endpoint.

#### Fix direction

- New package `internal/tui/wizard/picker/` with a generic `PickerStep[T]` (Go generics; Go 1.21+):
  - Constructed with: a title, a fetch function (`func(ctx) ([]T, error)`), a render function (`func(T) (primary, subtitle string)`), a state-key (`project_id`, `agent_id`, `provider_id`), and an optional "add new inline" handler that pushes the Sprint 3 CRUD form screen on top of the wizard stack.
  - Keys: Up/Down to navigate, Enter to select and advance (sets state, calls `Wizard.Next()`), `a` to add new inline (pushes Sprint 3 form; on form save, the new record is auto-selected and the user is returned to the picker's selected state).
- Concrete wiring in `internal/tui/wizard/launch/`:
  - Step 1: `PickerStep[config.Project]` bound to `client.ListProjects`, state key `project_id`.
  - Step 2: `PickerStep[config.Agent]` bound to `client.ListAgents`, state key `agent_id`.
  - Step 3: `PickerStep[config.Provider]` bound to `client.ListProviders`, state key `provider_id`.
- Inline search: each picker has a top-line `textinput` that filters the visible options via fuzzy match (reuse Sprint 1's matcher).

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/wizard/picker/` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/wizard/launch/` (new; wizard wiring)

#### Acceptance criteria

- [ ] Steps 1, 2, 3 all render as pickers with a filter input at the top.
- [ ] Enter on a selected row advances the wizard and stashes the pick in the wizard state map.
- [ ] `a` opens the matching Sprint 3 form on top of the wizard; on save, the new record is picked and the wizard is one step further along.
- [ ] Back nav returns to the picker with the previous selection highlighted.
- [ ] Filter state clears when re-entering a step (to avoid confusing stale filter results).

#### Test plan

- Unit: PickerStep tests (select + advance; `a` + add-new flow with a stub CRUD handler).
- Unit: wizard-state assertions after each step — project_id, agent_id, provider_id all present and correct.
- Manual: full run-through with and without inline-add usage.

#### Scope fences

- Do not allow multi-select (launch requires exactly one project / agent / provider).
- Do not implement cross-step defaults (e.g. "last-used agent per project"). Sprint 6 could add it.
- Do not show archived / deleted items.
- Do not let a picker advance with no selection — `Valid()` enforces.

#### Relationship

Depends on: T-v003-s04-01, T-v003-s03-02 / T-v003-s03-03 (inline add-new reuses CRUD forms).
Blocks: T-v003-s04-03.

#### Origin

User sketch: "Step 1 — pick project (filtered list). Step 2 — pick agent (filtered list, with `a` for add-new inline). Step 3 — pick provider."

---

### T-v003-s04-03: Step 4 — options / tools / permissions

**kind:** agent
**priority:** 2
**manual:** true
**tags:** [feature, tui, wizard, options]

#### Problem

After project/agent/provider are chosen, the user needs to compose the behavior knobs: prompt flags, env overrides, permission toggles, workspace mode. The Sprint 3 launch form already covers these fields — reuse it.

#### Fix direction

- New step type `OptionsStep` in `internal/tui/wizard/launch/options.go`:
  - Reuses Sprint 3's `form.Form` internally with the same field set as the Launch CRUD form, minus the `project` / `agent` / `provider` fields (those come from earlier steps).
  - Defaults are seeded from the chosen agent (permissions.default_sandbox, etc.) and provider where applicable.
  - `Valid()` checks that any mandatory options are set (minimal — workspace mode if required).
  - `Summary()` returns a multi-line string showing the chosen values for the review step.
- Honors the Sprint 3 T-06 toggle audit: every bool-valued option renders as a ToggleField.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/wizard/launch/options.go` (new)

#### Acceptance criteria

- [ ] Step 4 renders a form with every non-identity field of `config.Launch`.
- [ ] Defaults are pre-populated from the chosen agent / provider where reasonable.
- [ ] Tab / Shift-Tab navigation within the form works.
- [ ] Bool fields render as toggles.
- [ ] Back nav preserves the form's field values; forward nav re-seeds defaults only on first entry.

#### Test plan

- Unit: default-seeding test — for a given (project, agent, provider) triple, assert expected default values.
- Unit: `Summary()` format test.
- Manual: step through the wizard, toggle prompt flags, back up, confirm the toggles persist.

#### Scope fences

- Do not add tool-selection UX beyond what the Sprint 3 launch form already exposes.
- Do not add environment-variable validation beyond Sprint 3's `KEY=VALUE` parser.
- Do not expose provider-specific advanced options in v0.0.3.
- Do not persist options across wizard restarts.

#### Relationship

Depends on: T-v003-s04-01, T-v003-s04-02, T-v003-s03-04.
Blocks: T-v003-s04-04.

#### Origin

User sketch: "Step 4 — options / tools / permissions (toggles for prompt flags, env overrides, sandbox profile, permission toggles). Reuse Sprint 3 form framework."

---

### T-v003-s04-04: Step 5 — review + three exit actions

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [feature, tui, wizard, review]

#### Problem

The review step is where the wizard decides the user's intent: try it once, save it and try it, or save it for later. This is the step the user explicitly called out: "`l` launch ephemeral, `s` save-and-launch, `S` save-only."

#### Fix direction

- New step type `ReviewStep` in `internal/tui/wizard/launch/review.go`:
  - View renders the composed launch plan by calling `Summary()` on every prior step and framing each in a small Lipgloss panel.
  - Footer shows: `← back · l launch · s save+launch · S save only · Esc cancel`.
  - Key handlers:
    - `l` — compose an inline launch payload from wizard state and POST to the daemon's ephemeral-launch endpoint (`POST /sessions` with inline plan). On success, pop the wizard and push a toast `Launched ephemeral session <id>`.
    - `s` — compose a `config.Launch`, call `client.CreateLaunch(...)`, then call `client.CreateAndLaunch(launch_id)`. On success, pop and toast `Saved and launched <launch-id>`.
    - `S` — same as `s` but no launch call; toast `Saved launch <launch-id>`.
    - Esc — cancel-confirmation (shared with T-01).
  - Errors: on any failure, show an error modal with the message and keep the wizard on the review step so the user can retry or back up to fix.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/wizard/launch/review.go` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/client/launch.go` — wrapper for the ephemeral-plan payload (see T-05 for contract)

#### Acceptance criteria

- [ ] Review step renders a formatted summary of the composed launch.
- [ ] `l`, `s`, `S` each trigger the correct backend call and the correct post-action UX.
- [ ] On success, the wizard pops and the main screen is visible.
- [ ] On error, the review step stays and surfaces the error.
- [ ] `← back` returns to Step 4 with values preserved.

#### Test plan

- Unit: each exit path test with a stub client (success + error).
- Integration: end-to-end run of each exit path against a real daemon; verify YAML is / isn't written per exit.
- Manual: the cancel-confirmation from Esc still applies even on the review step.

#### Scope fences

- Do not name-generate launch IDs server-side here — the user must provide an ID for the save paths. If not provided, show a modal asking for it.
- Do not implement a "diff view" between the composed plan and an existing launch. That's a Sprint 6+ enhancement.
- Do not support partial saves (some fields saved, others not).
- Do not silently accept launch failure — always surface.

#### Relationship

Depends on: T-v003-s04-01, T-v003-s04-02, T-v003-s04-03, T-v003-s04-05.
Pairs with: T-v003-s03-04 (uses `CreateLaunch` from Sprint 3).

#### Origin

User sketch: "Step 5 — review screen. Three exit actions: `l` launch ephemeral (no save), `s` save-and-launch, `S` save-only."

---

### T-v003-s04-05: Ephemeral-launch client + endpoint contract

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [feature, tui, api-client, ephemeral-launch]

#### Problem

Quick-launch in Sprint 1 sends `{launch_id: "..."}`. The wizard's ephemeral path must send an inline plan with no `launch_id`. If v002-s05's `POST /sessions` doesn't accept that shape, this task is a readiness gap that forces a v0.0.2 addendum.

#### Fix direction

- Define the inline-plan request struct in `internal/tui/client/launch.go`:
  ```
  CreateAndLaunchRequest struct {
      LaunchID   string         // present for saved-launch path
      InlinePlan *InlineLaunchPlan // present for ephemeral path
  }
  InlineLaunchPlan struct {
      ProjectID, AgentID, ProviderID string
      Workspace config.LaunchWorkspace
      Prompt    config.PromptSpec
      Overrides config.LaunchOverrides
  }
  ```
- Serialization: either `launch_id` or `plan` at the top level. Document the contract in the client README and the daemon API reference (v002-s05-06).
- Verify the daemon endpoint accepts the shape. Options:
  - (a) v002-s05-01 already accepts an inline plan — confirm and proceed.
  - (b) v002-s05-01 only accepts `launch_id` — add a task to v0.0.2's backlog and flag as a readiness blocker.
  - (c) Sprint 3's catalog-CRUD decision ended up at "write YAML + reload"; then ephemeral = write transient YAML + launch + delete. This is a sub-optimal path; prefer (a).
- On the TUI side, expose `client.LaunchEphemeral(plan)` as an alias for `CreateAndLaunch({InlinePlan: &plan})`.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/client/launch.go` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/client/README.md` — document the contract
- `/Users/chrispian/Projects-apps/agent-mux-v0-pack/docs/adr/` (new ADR if a v0.0.2 addendum is required)

#### Acceptance criteria

- [ ] `client.LaunchEphemeral(plan)` works against the daemon with no prior YAML write.
- [ ] The request payload shape is documented in the client README and matches the API reference.
- [ ] If the endpoint doesn't support the shape, a readiness gap is filed and T-04's `l` exit is flagged as blocked.
- [ ] Unit tests cover the JSON serialization for both `LaunchID` and `InlinePlan` variants.

#### Test plan

- Unit: JSON round-trip test for both variants.
- Integration: launch ephemerally against a real daemon; confirm no YAML file was created under `~/.agent-mux/catalog/launches/`.
- Manual: run through the wizard's ephemeral exit and verify the session is visible on the main screen afterwards, with no corresponding launch profile.

#### Scope fences

- Do not implement server-side validation here — this is client work only.
- Do not conflate the ephemeral payload with the save-and-launch payload (save path writes a YAML first).
- Do not add ephemeral-only fields (no "ephemeral" flag on the record; absence of `launch_id` is the signal).
- Do not fall back to the save path if ephemeral support isn't there — surface as a readiness gap.

#### Relationship

Depends on: v002-s05-01 (the endpoint this exercises), T-v003-s04-04 (consumer).
Pairs with: Sprint 5's checkpoint-resume (which has a similar inline-plan pattern for resume-with-overrides).

#### Origin

Epic v0.0.3 decision D6: "Ephemeral launches are allowed — user doesn't need to save a launch YAML first." User sketch: "Launch-only (ephemeral)."

## Review / readiness notes

- **Ephemeral-launch endpoint confirmation.** This is the one hard dependency this sprint has on v0.0.2. If v002-s05's `POST /sessions` only takes `launch_id`, a follow-up v0.0.2 task must extend it, or Sprint 4 must ship with ephemeral disabled (launch-only exit greyed out with a "daemon doesn't support this yet" message). Record the decision in an ADR.
- **Add-new inline flow quirk.** T-02's `a` key pushes a full CRUD form on top of the wizard stack. If the user cancels the form, the wizard state is preserved (because the form never committed). If the user saves, the new record auto-selects. The screen-stack plumbing from Sprint 2 must handle this cleanly — sanity-check during readiness.
- **Draft persistence.** Per epic readiness, wizard state does NOT survive a TUI restart. Revisit in v0.0.5+ if the complaint rate is high.
- **Launch ID generation.** T-04's save paths require the user to name the launch. If the review step can auto-suggest (e.g. `{project}-{agent}-{short-date}`), add that in a small follow-up task; keep explicit naming as a fallback.
- **Wizard-from-launch-row UX.** Should Right-arrow on a launch row open the wizard pre-populated from that launch? Tempting but out of scope for Sprint 4. Capture as a backlog enhancement.
- **Validation surface at review.** T-04's review step is where the user finds out the plan is invalid. Ideally, Steps 1–4 would catch most issues, but API-level validation (e.g. provider not reachable) only fires on submit. The error modal on the review step has to be informative, not just `error 400`.
