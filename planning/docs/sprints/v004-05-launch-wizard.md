# Sprint v004-05 — Launch Wizard (pulled from v0.0.3 Sprint 4)

Epic: [v0.0.4](../epics/v0.0.4-dogfood-ready-infrastructure.md)
Lineage: originally specced as `docs/sprints/v003-04-launch-wizard.md`. Pulled forward into v0.0.4 because ergonomic ad-hoc launches matter for dogfooding velocity. The original sprint file stays as-is for reference; this file supersedes it for execution.

**Reordered 2026-04-19** — was v004-04; became v004-05 when the claudestream-provider sprint slotted in at position 2. Scope unchanged. The wizard must now dispatch on provider kind (PTY vs stream-json) for the auto-attach flow after launch.

**Goal:** Multi-step wizard for composing a session launch: pick project → pick agent → pick provider → set options → review. Three exits from the review step: **ephemeral** (launch-only, no save), **save-and-launch** (persist a new launch profile + start), **save-only** (persist the profile without starting).

**Exit criteria:**
- [ ] `mux tui` has a wizard entry point — palette verb `new launch` or key `n` on main-screen empty state.
- [ ] Each step validates before advancing.
- [ ] Ephemeral exit posts directly to the session-create endpoint with an inline plan — no catalog mutation.
- [ ] Save-and-launch exit writes a new launch profile AND starts a session from it.
- [ ] Save-only exit writes the profile without starting. TUI pops back to main with a toast.
- [ ] Workspace mode / overrides are editable via the options step.
- [ ] `make check` green.

## Readiness gap (resolve before T-03)

**Ephemeral-launch endpoint shape.** `POST /sessions` today requires a `launch_id` referencing an existing catalog launch profile. Ephemeral launches need an alternate shape where the full launch plan (project / agent / provider / overrides) is inlined in the request body. Three resolution paths:

- **(A)** Extend `POST /sessions` to accept either `launch_id` OR an inline plan object. Union on the request DTO. Cleanest.
- **(B)** Add a new `POST /sessions:launch-ephemeral` route with its own DTO. Avoids overloading the existing endpoint.
- **(C)** TUI writes an ephemeral launch profile to a temp location, uses it via `launch_id`, cleans it up on exit. Ugly; avoid.

Capture the decision as ADR 0016 during T-02 readiness pass (0013 = sandbox, 0014 = pty-resize, 0015 = checkpoint, 0016 = ephemeral-launch). My leaning: (A). Simpler for clients; the DTO union is cheap to validate.

Also: **save-only** exits don't need a daemon call at all — they just write YAML to the catalog dir. But the daemon needs to be told to re-scan, so either:
- Save-only calls `POST /catalog/reload` (doesn't exist yet either), or
- Save-only writes then the TUI re-fetches via `GET /catalog/launches` to confirm.

The second path is simpler for MVP; capture as a readiness note.

## Tasks

Copy and adapt from `docs/sprints/v003-04-launch-wizard.md` (the original wizard sprint file). Adaptations for v0.0.4:

- Use the form-modal framework from Sprint v004-03 T-03 if it generalized to multi-field forms; otherwise build a step-based wizard screen from scratch.
- Checkpoint-resume integration from Sprint v004-03 makes the "review step" slightly richer: a wizard-launched session can be checkpointed same as any other.
- Ephemeral launches use whichever endpoint path was decided in the readiness gap above.

Task breakdown sketch:

- **T-v004-s04-01** — Wizard screen stack framework (multi-step Screen, per-step validation, back/next, cancel)
- **T-v004-s04-02** — Ephemeral-launch endpoint (ADR 0016 + daemon route + client method)
- **T-v004-s04-03** — Step 1: project selection (reuse main-screen search against `ListProjects`)
- **T-v004-s04-04** — Step 2: agent selection
- **T-v004-s04-05** — Step 3: provider selection
- **T-v004-s04-06** — Step 4: options (workspace mode + env overrides)
- **T-v004-s04-07** — Step 5: review + three-exit dispatch
- **T-v004-s04-08** — Save-only path: YAML write + catalog re-scan trigger

## Review / readiness notes

- **Step-model ergonomics.** If the Screen interface's `Update` passthrough feels awkward for multi-step state, consider a `WizardScreen` composite that itself holds sub-screens. Don't over-engineer — a sequential state machine with explicit `step int` + a `stepState` struct per step is probably enough.
- **Palette verb registration.** Sprint v003-s02-03 established `registerDefaultVerbs`. Add a `new launch` verb from this sprint's code so it shows up in the palette without touching the Sprint 2 verb registration path.
- **Validation error display.** Each step has its own validation; errors render inline at the bottom of the step panel (not as toasts, which disappear). Toasts are for cross-step confirmation (save-only success, launch success).
