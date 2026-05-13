# Sprint v003-03 — Config CRUD via Forms

Epic: [v0.0.3](../epics/v0.0.3-tui-mvp.md)

**Epic:** [v0.0.3](../epics/v0.0.3-tui-mvp.md)
**Goal:** Let the user create, edit, and delete every catalog-owned object — projects, agents, providers, launches — without leaving the TUI. Introduce a reusable keyboard-driven form framework (labeled text / multiline / select / toggle / string-list fields) and wire CRUD screens on top of it for each type. Surface API validation errors inline; guard against data loss on accidental Esc.
**Exit criteria:**
- [ ] Form framework under `internal/tui/form/` supports text, multiline, select/enum, toggle, and string-list field kinds. Keyboard navigation (Tab / Shift-Tab / arrows / Enter / Esc) is consistent across all field kinds.
- [ ] Project, Agent, Provider, and Launch each have working list + new + edit + delete flows. Keys follow user convention: `a` (or `i`) to add new on a list, `e` to edit the selected row, `d` (with confirmation) to delete, `s` to save the current form.
- [ ] API validation errors render inline next to the offending field with a distinctive style.
- [ ] Attempting to Esc out of a form with unsaved changes shows a confirmation modal (`Discard changes? (y/N)`).
- [ ] Fields that are obviously toggles (e.g. `permissions.network`, `prompt.include_project_boot`, `prompt.include_agent_boot`, `prompt.include_knowledge_base`) render as a toggle widget, not a text input.

## Context

Sprint 2 gave every catalog object a read-only detail view. This sprint makes them mutable. The catalog object model lives in `/Users/chrispian/Projects-apps/agent-mux/internal/config/model.go`; each form field maps to a struct field on one of `Project`, `Agent`, `Provider`, or `Launch`. The daemon API must expose catalog-CRUD endpoints for this sprint to land — see the epic's readiness notes.

Current shape to evolve:
- `/Users/chrispian/Projects-apps/agent-mux/internal/config/model.go` — source of truth for field shapes.
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/detail/` (from Sprint 2) — read-only; this sprint adds `internal/tui/form/` siblings.
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/client/` — add Create/Update/Delete methods per type.
- Catalog-CRUD endpoint decision: Option (a) new endpoints in v0.0.2, Option (b) TUI writes YAML + calls reload, Option (c) v0.0.2 Sprint 8. **This sprint is blocked until that decision is ADR'd.**

When done, the TUI is self-sufficient for catalog authoring: a new user can do first-run setup entirely from the wizard and these forms without hand-editing YAML.

## Tasks

### T-v003-s03-01: Reusable form framework

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [feature, tui, form-framework]

#### Problem

Each of the four CRUD screens needs labeled fields, focus management, validation state, and unsaved-changes tracking. Reimplementing this per screen yields four incompatible UX dialects. A small framework keeps every form coherent.

#### Fix direction

- New package `internal/tui/form/`:
  - `Field` interface — `Focus()`, `Blur()`, `View() string`, `Value() any`, `SetValue(any) error`, `Dirty() bool`, `Validate() error`.
  - Concrete fields: `TextField` (single-line, Bubbles `textinput`), `MultilineField` (Bubbles `textarea`), `SelectField` (enum picker — arrow-keys to cycle, Enter to confirm, a small inline dropdown), `ToggleField` (on/off, Space to toggle), `StringListField` (list of strings with `+` to add, `-` to remove, Enter to edit entry, arrow keys to reorder).
  - `Form` struct holding `[]Field`, focused-index pointer, submit handler (`func() tea.Cmd`).
  - Keyboard: Tab / Down advances focus; Shift-Tab / Up retreats; `s` from a non-input-focused context submits; Esc triggers unsaved-changes guard then pops the screen.
  - Dirty tracking: each field compares current value to initial value; form `Dirty()` returns true if any field is dirty.
- Lipgloss styling: focused field has accent border; unfocused is muted; error messages render in a distinctive error color underneath the field.
- Validation surface: `Form.SetFieldError(fieldName string, err error)` — attaches an error to a specific field, typically populated from an API 400 response.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/form/` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/form/README.md` (new; field-kind reference + examples)

#### Acceptance criteria

- [ ] Each field kind exists with its own file (`text.go`, `multiline.go`, `select.go`, `toggle.go`, `stringlist.go`).
- [ ] Tab / arrow navigation advances focus across field kinds; focus state is visually obvious.
- [ ] `StringListField` supports add (`+` or `a`), remove (`-` or `d`), reorder (Alt-Up / Alt-Down), and edit-entry (Enter).
- [ ] `Form.Dirty()` flips to true when any field is mutated and back to false after submit succeeds.
- [ ] `SetFieldError` renders the error under the named field with error-color styling.

#### Test plan

- Unit: per-field tests covering set/get/dirty/validate.
- Unit: form-focus tests covering Tab / Shift-Tab across mixed field kinds.
- Unit: `SetFieldError` + `ClearFieldError` tests.
- Manual: a demo form with one of each field kind to eyeball styling and focus behavior.

#### Scope fences

- Do not implement number / date / URL field kinds. The catalog model only needs text, multiline, select, toggle, and string-list.
- Do not implement form-wide validation (cross-field rules) here. API-response-driven error surfacing is enough for v0.0.3.
- Do not add mouse support.
- Do not implement undo / redo per field. Esc with discard-confirmation is the rollback path.

#### Relationship

Blocks: T-v003-s03-02, T-v003-s03-03, T-v003-s03-04, every Sprint 4 wizard task that reuses form fields, Sprint 5 LogicalAgent form.

#### Origin

User sketch: "CRUD all config objects — with a consistent toggle widget, not a text field." Epic v0.0.3 exit criteria: full CRUD across catalog objects.

---

### T-v003-s03-02: Project CRUD screens

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [feature, tui, crud, projects]

#### Problem

No way to create or edit a project from the TUI. Users hand-edit `projects.yaml`, which is friction-heavy and error-prone.

#### Fix direction

- New package `internal/tui/crud/projects/`:
  - `list_screen.go` — derived from Sprint 1 main-screen's project filter but a dedicated screen reachable via palette verb `view projects` or as a sub-screen. Keys: `a` new, `e` edit on selected row, `d` delete with confirmation (reuses Sprint 2's modal), `Enter` opens detail, `s` is a no-op here (reserved for save-in-form).
  - `form_screen.go` — wraps a `form.Form` configured with fields matching `config.Project`:
    - `id` — TextField, required, validated on submit
    - `name` — TextField, required
    - `repo_root` — TextField (path)
    - `tracking_root` — TextField (path)
    - `knowledge_base` — StringListField (paths)
    - `boot_fragments` — StringListField
    - `workspace.default_mode` — SelectField (values sourced from existing `WorkspaceSpec` enumeration)
    - `workspace.worktree_base` — TextField
    - `workspace.session_root` — TextField
- Submit: `client.CreateProject(p)` for new, `client.UpdateProject(p)` for edit. On success pop back to the project list and show a toast.
- Delete: confirmation modal → `client.DeleteProject(id)` → toast + list refresh.
- Palette verbs registered: `add project`, `edit project`, `delete project`.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/crud/projects/` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/client/` — add `CreateProject` / `UpdateProject` / `DeleteProject`
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/wire.go` — register palette verbs

#### Acceptance criteria

- [ ] `a` on the project list opens an empty form.
- [ ] `e` on a selected project row opens a pre-populated form.
- [ ] `s` inside the form submits; successful submit pops back to the list and toasts.
- [ ] `d` on a selected row shows confirmation; on `y`, deletes and refreshes.
- [ ] Validation errors from the API (missing required field, duplicate ID) render inline against the offending field.
- [ ] The list view updates without requiring a full TUI restart.

#### Test plan

- Unit: form-submit path tests with a stub client (success + validation-error).
- Integration: against a running daemon with catalog CRUD enabled, run the full add / edit / delete cycle; assert the on-disk YAML reflects changes.
- Manual: verify with a seeded catalog that edits persist and round-trip via `mux projects list` CLI.

#### Scope fences

- Do not support bulk-edit or multi-select here.
- Do not implement import/export of projects in this sprint.
- Do not add revision history. Last-write-wins per epic readiness note.
- Do not auto-fill `id` from `name` — explicit ID entry is fine for MVP.

#### Relationship

Depends on: T-v003-s03-01, catalog-CRUD API decision (see epic readiness notes).
Siblings: T-v003-s03-03, T-v003-s03-04 (same pattern per type).

#### Origin

Epic v0.0.3 exit criteria: full CRUD across catalog objects. User sketch: "CRUD all config objects: projects, agents, providers, launches."

---

### T-v003-s03-03: Agent + Provider CRUD screens

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [feature, tui, crud, agents, providers]

#### Problem

Same motivation as T-02, applied to `Agent` and `Provider`. These share enough shape with project CRUD that the pattern is copy-adapt but they have distinct fields (agent permissions, provider bootstrap mode, provider env passthrough) that need dedicated widgets.

#### Fix direction

- New package `internal/tui/crud/agents/` mirroring the projects package:
  - Agent form fields: `id`, `name`, `roles` (StringList), `skills` (StringList), `context_files` (StringList), `boot_fragments` (StringList), `permissions.network` (Toggle), `permissions.default_sandbox` (Select — values from existing sandbox profile list).
- New package `internal/tui/crud/providers/`:
  - Provider form fields: `id`, `type` (Select — `cli` / `api` to cover both v002-s04 runtime modes), `command`, `args` (StringList), `bootstrap.mode` (Select), `bootstrap.prompt_prefix` (Multiline), `env.passthrough` (StringList).
- Both reuse Sprint 2's confirmation modal for delete. Both reuse T-01's form framework end-to-end.
- Palette verbs: `add agent`, `edit agent`, `delete agent`, `add provider`, `edit provider`, `delete provider`.
- `permissions.network` and `bootstrap.mode` are the first real consumers of the ToggleField and SelectField — flag any form-framework gaps surfaced here as follow-ups on T-01.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/crud/agents/` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/crud/providers/` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/client/` — add `CreateAgent` / `UpdateAgent` / `DeleteAgent` / same for Provider
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/wire.go` — register palette verbs

#### Acceptance criteria

- [ ] Agent CRUD supports every field of `config.Agent` including `AgentPermissions`.
- [ ] Provider CRUD supports every field of `config.Provider` including `BootstrapSpec` and `ProviderEnv.Passthrough`.
- [ ] `permissions.network` is a toggle, not a text field.
- [ ] `bootstrap.mode` is a select with the documented enum values.
- [ ] Validation errors render inline (same UX as projects).
- [ ] Delete confirmation modal is shared with projects (no duplication).

#### Test plan

- Unit: form-submit path tests per type (stub client).
- Integration: add / edit / delete each type; assert on-disk YAML shape matches `config.Agent` / `config.Provider`.
- Manual: confirm the toggle and select widgets read and write the expected values.

#### Scope fences

- Do not introduce a separate "secrets" editor for provider env — plaintext list is fine for v0.0.3 (env passthrough is keys, not values).
- Do not auto-detect provider type from the `command` binary.
- Do not wire agents to LogicalAgent in this sprint — LogicalAgent is a runtime concept handled in Sprint 5.
- Do not add agent / provider search beyond what the main screen already provides.

#### Relationship

Depends on: T-v003-s03-01, T-v003-s03-02 (establishes the CRUD pattern), catalog-CRUD API decision.
Siblings: T-v003-s03-04.

#### Origin

Epic v0.0.3 exit criteria: full CRUD across catalog objects. `internal/config/model.go` field list.

---

### T-v003-s03-04: Launch CRUD screens

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [feature, tui, crud, launches]

#### Problem

Launch is the CRUD object with the most sub-structure: a launch binds project + agent + provider + workspace mode + prompt flags + env overrides. It's also the highest-traffic CRUD (every new launch profile goes through this form).

#### Fix direction

- New package `internal/tui/crud/launches/`:
  - Launch form fields:
    - `id` — TextField, required
    - `project` — SelectField populated from `client.ListProjects`
    - `agent` — SelectField populated from `client.ListAgents`
    - `provider` — SelectField populated from `client.ListProviders`
    - `workspace.mode` — SelectField
    - `workspace.worktree_name` — TextField
    - `workspace.write_home` — TextField
    - `prompt.include_project_boot` — ToggleField
    - `prompt.include_agent_boot` — ToggleField
    - `prompt.include_knowledge_base` — ToggleField
    - `overrides.env` — StringListField of `KEY=VALUE` entries (document the format; parse on submit)
- Select fields must refresh their options when the user opens the form (so a project added 30 seconds ago is selectable).
- Palette verbs: `add launch`, `edit launch`, `delete launch`.
- On successful save, the list view refreshes so the new launch immediately becomes quick-launchable from the Sprint 1 main screen.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/crud/launches/` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/client/` — add `CreateLaunch` / `UpdateLaunch` / `DeleteLaunch`
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/wire.go` — register palette verbs

#### Acceptance criteria

- [ ] `add launch` opens a fully-populated form where every selectable field has current catalog options.
- [ ] Each of the three prompt flags renders as a toggle.
- [ ] `overrides.env` accepts `KEY=VALUE` entries; malformed entries surface as inline errors.
- [ ] After save, the new launch is immediately visible on the main screen and quick-launch works on it.
- [ ] Delete shows the confirmation modal and updates the list.

#### Test plan

- Unit: env-entry parser tests (`KEY=VALUE`, rejects malformed).
- Unit: select-refresh test — a project added between form-open and form-submit is picked up.
- Integration: create a launch; verify `mux launch --launch <new-id>` works against it from the CLI.
- Manual: edit an existing launch, toggle one prompt flag, save, launch it, confirm the launched session reflects the change.

#### Scope fences

- Do not implement launch "clone" in this task. A user can copy an existing launch manually.
- Do not support env value masking (secrets UX).
- Do not support multi-line env values (single-line `KEY=VALUE` only).
- Do not expose provider-specific overrides beyond `overrides.env` in v0.0.3.

#### Relationship

Depends on: T-v003-s03-01, T-v003-s03-02, T-v003-s03-03 (select fields consume the list endpoints), catalog-CRUD API decision.
Reused by: Sprint 4 wizard's save-and-launch / save-only exits.

#### Origin

Epic v0.0.3 exit criteria: full CRUD across catalog objects; launch is the primary launch-authoring surface.

---

### T-v003-s03-05: Inline validation + unsaved-changes guard

**kind:** agent
**priority:** 2
**manual:** true
**tags:** [feature, tui, form-framework, validation]

#### Problem

Submitting a form with invalid data yields an API 400. T-01 sketches `SetFieldError`, but each CRUD screen needs a consistent pipeline from "API responded 400 with field errors" to "render those under the right fields." And Esc from a dirty form must not silently discard minutes of typing.

#### Fix direction

- Define a canonical validation-error payload shape from the daemon: `{error: {code: "validation_error", fields: [{field: "id", message: "…"}]}}`. (v002-s05's response envelope decision covers this; if fields aren't structured, negotiate a v0.0.2 follow-up or parse the error message.)
- Client layer: `ValidationError` type exposed from `internal/tui/client/` carries `Fields []FieldError`.
- Form screens: after a submit call, if the error is `ValidationError`, iterate its fields and call `form.SetFieldError(name, msg)` for each. Keep focus on the first error field.
- Unsaved-changes guard: generalize Sprint 2's confirmation modal. When the user presses Esc on a form with `Dirty() == true`, show `Discard changes? (y/N)`. `y` pops the screen; `N` / Esc dismisses the modal and returns to the form.
- The guard is a form-framework concern, not a per-screen one — it's implemented once inside `form.Form`'s Esc handler.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/client/errors.go` — add `ValidationError`
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/form/form.go` — unsaved-changes guard
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/crud/` — adopt ValidationError handling in each subpackage

#### Acceptance criteria

- [ ] Submitting a form with invalid data surfaces per-field errors inline without clearing user input.
- [ ] Focus lands on the first error field after a validation failure.
- [ ] Esc from a dirty form always shows the discard-confirmation modal.
- [ ] Esc from a clean form pops immediately (no modal noise).
- [ ] A successful submit clears all field errors before navigating away.

#### Test plan

- Unit: error-mapping test — given a canned `ValidationError`, assert `SetFieldError` is called for every field with matching messages.
- Unit: Esc-guard tests in both dirty and clean form states.
- Manual: submit a project form with a duplicate ID; confirm the UX.
- Manual: start editing an agent, press Esc, confirm the modal, pick `N`, verify state preserved.

#### Scope fences

- Do not add client-side validation in this task. Rely on API feedback.
- Do not auto-retry failed submits.
- Do not implement optimistic updates.
- Do not add a "save as draft" feature.

#### Relationship

Depends on: T-v003-s03-01 (`SetFieldError` plumbing), T-v003-s03-02 / T-v003-s03-03 / T-v003-s03-04 (consumers).

#### Origin

User convention: forms should not eat work. Epic v0.0.3 exit criteria: inline validation feedback.

---

### T-v003-s03-06: Settings-toggle widget consistency audit

**kind:** agent
**priority:** 3
**manual:** true
**tags:** [polish, tui, form-framework, toggles]

#### Problem

The user called this out explicitly: fields that are semantically booleans (permissions toggles, prompt flags) must render as toggles, not as `true`/`false` text inputs. A drift here early makes the whole app feel sloppy.

#### Fix direction

- Enumerate every boolean field across `config.Project`, `config.Agent`, `config.Provider`, `config.Launch` (and, looking ahead, `LogicalAgent` hot/cold policy from Sprint 5):
  - `config.AgentPermissions.Network` → toggle
  - `config.PromptSpec.IncludeProjectBoot` / `IncludeAgentBoot` / `IncludeKnowledgeBase` → toggle
  - (Sprint 5) `LogicalAgent.HotCold` if it's bool-ish → toggle or select depending on the final shape.
- Audit each CRUD screen in this sprint and assert the right widget is used.
- Add a lint-style test: iterate the registered form field definitions, assert any field whose value type is `bool` uses `ToggleField`. Violations fail the test.
- Refine the `ToggleField` styling: accent-colored "on" state, muted "off" state, `Space` or Enter to flip, clear labelling (`[x] network` vs `[ ] network`).
- Document the pattern in `internal/tui/form/README.md` so Sprint 5 extensions follow it.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/form/toggle.go` — styling refinements
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/form/form_audit_test.go` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/form/README.md` — pattern documentation

#### Acceptance criteria

- [ ] Every bool-valued catalog field in `internal/config/model.go` renders as `ToggleField` in the CRUD screens.
- [ ] The audit test fails fast if a future field is added as TextField when it should be ToggleField.
- [ ] Toggle rendering is visually distinct from text input at a glance.
- [ ] `Space` and Enter both flip a focused toggle; arrow keys do not.

#### Test plan

- Unit: audit test.
- Unit: toggle-interaction test (Space and Enter flip; Tab does not).
- Manual: walk through all four CRUD screens and verify toggle fields are visually correct.

#### Scope fences

- Do not convert SelectField enums to toggles even if they have two values — enums may grow a third value later.
- Do not remove toggles from YAML in favor of deriving them from state.
- Do not add an "indeterminate" tri-state toggle.
- Do not change the underlying YAML schema.

#### Relationship

Depends on: T-v003-s03-01, T-v003-s03-02, T-v003-s03-03, T-v003-s03-04.
Pairs with: T-v003-s05-01 (LogicalAgent hot/cold flag — apply the same audit).

#### Origin

User call-out: "Use a consistent toggle widget, not a text field."

## Review / readiness notes

- **Catalog-CRUD API is the blocker.** This sprint cannot start until the epic-level decision between (a) new v0.0.2 Sprint 5 catalog endpoints, (b) TUI-writes-YAML + reload endpoint, or (c) a new v0.0.2 Sprint 8 is recorded in an ADR. If the decision is (b), every `client.Create*` / `client.Update*` / `client.Delete*` method writes a local YAML and POSTs `/catalog/reload`; if (a) or (c), methods hit typed endpoints.
- **ID collision handling.** Multiple catalog YAML files can share a directory; T-02/03/04's create flow needs an explicit rule for where the new YAML lands. Suggest: one file per object in `~/.agent-mux/catalog/<type>s/<id>.yaml`. Confirm at readiness.
- **Concurrent-edit race.** Two TUIs (or a TUI and the CLI) editing the same object produce last-write-wins silently. Epic-level readiness note acknowledges the gap. If this sprint uncovers a pattern where the UX is actively harmful (e.g. stale field values in the form), escalate to epic-level.
- **API response envelope.** T-05 assumes structured field errors. If v0.0.2 picked RFC 7807 without a `fields` extension, parse the `detail` text or add a small extension. Flag before T-05 runs.
- **Env-entry format.** T-04 chose `KEY=VALUE` lines for `overrides.env`. If user testing surfaces a desire for multi-line values or quoted values, revisit in v0.0.5+.
- **Sandbox-profile enum source.** T-03's agent form references an enumeration of sandbox profiles. That list lives somewhere in the existing codebase (or needs to be introduced). Confirm or extract at readiness.
- **Sprint sequencing.** T-01 blocks everything else. T-02 → T-03 / T-04 can run in parallel after T-01 lands. T-05 and T-06 are polish and run last.
