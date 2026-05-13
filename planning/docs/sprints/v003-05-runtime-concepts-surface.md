# Sprint v003-05 — Runtime Concepts Surface

Epic: [v0.0.3](../epics/v0.0.3-tui-mvp.md)

**Epic:** [v0.0.3](../epics/v0.0.3-tui-mvp.md)
**Goal:** Expose the v0.0.2 runtime concepts in the TUI: `LogicalAgent`, `Checkpoint`, `BrokerEnvelope`. Add the unified universal-search view the user sketched — a single main screen that mixes catalog objects, runtime objects, and sessions. Add an optional event-stream side panel so the user can observe the runtime's internal activity while working.
**Exit criteria:**
- [ ] LogicalAgent has list + detail + CRUD screens. Hot/cold policy, permitted tools, and memory-scope list are editable.
- [ ] Checkpoint has list + detail + resume surfaces. Each checkpoint shows summarized context, pending work, and key decisions; resume is a one-key action that posts to `POST /logical-agents/{id}/resume`.
- [ ] BrokerEnvelope has list + detail + compose + reply surfaces. List is filterable by thread / correlation-id / recipient. Compose and reply both use the form framework.
- [ ] Main screen universal search mixes projects / agents / providers / launches / sessions / logical-agents / checkpoints / broker-envelopes. Filter chips gain per-type toggles for the new types.
- [ ] An event-stream side panel is toggleable via palette verb (`show events`) or a keybinding (e.g. `E`). It subscribes to `GET /events/stream` and renders the last N events.

## Context

Sprint v002-03 adds `logical_agents`, `checkpoints`, and `broker_envelopes` tables; Sprint v002-05 adds their API endpoints. Sprint v0.0.3 fills in resume and request/reply semantics. This sprint makes those concepts visible and manipulable from the TUI so the runtime stops being a black box from a user's perspective.

Current shape to evolve:
- v002-s03 storage: `logical_agents`, `checkpoints`, `broker_envelopes` tables.
- v002-s05 endpoints: `POST /sessions/{id}/checkpoint`, `GET /logical-agents/{id}/checkpoints`, `POST /logical-agents/{id}/resume`, `POST /broker/envelopes`, `GET /broker/envelopes`, `GET /broker/envelopes/{id}`, `POST /broker/envelopes/{id}/reply`, `GET /events/stream`.
- v0.0.3 fills in the semantic depth (resume actually restores context; reply actually correlates).
- `/Users/chrispian/Projects-apps/agent-mux/internal/store/` — logical-agent / checkpoint / broker-envelope storage types live here once Sprint v002-03 lands.
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/form/` (Sprint 3) reused throughout.

When done, the TUI is the go-to surface for inspecting everything the daemon owns — not just launches and sessions but the richer runtime graph behind them.

## Tasks

### T-v003-s05-01: LogicalAgent CRUD

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [feature, tui, crud, logical-agent]

#### Problem

`LogicalAgent` is the durable-identity concept v002-s03 introduces. Today the TUI shows nothing about it. Without a surface, users can't manage hot/cold policy or permitted tools.

#### Fix direction

- New package `internal/tui/crud/logicalagents/`:
  - List screen: one row per logical agent with id, name, hot/cold state, session count.
  - Detail screen: full field view including permitted tools, memory scopes, hot/cold policy.
  - Form screen: wraps `form.Form` with fields matching whatever shape `store.LogicalAgent` lands in v002-s03. At minimum: `id`, `name`, `hot_cold_policy` (Select or Toggle — see Sprint 3 T-06 audit), `permitted_tools` (StringList), `memory_scopes` (StringList).
- Palette verbs: `view logical-agents`, `add logical-agent`, `edit logical-agent`, `delete logical-agent`.
- API client extension: `client.ListLogicalAgents`, `client.GetLogicalAgent`, `client.CreateLogicalAgent`, `client.UpdateLogicalAgent`, `client.DeleteLogicalAgent`.
- Main-screen integration: `LogicalAgentRow` added to the result-row union; filter chip `[logical-agents]` (or abbreviated `[la]` if row width is tight).

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/crud/logicalagents/` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/client/logicalagents.go` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/row.go` — add `LogicalAgentRow`
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/wire.go` — register palette verbs

#### Acceptance criteria

- [ ] List / detail / form / delete all work for LogicalAgent.
- [ ] Hot/cold policy renders as a toggle or select matching the v002-s03 type (honor Sprint 3 T-06 audit).
- [ ] Permitted-tools and memory-scopes use the StringListField from Sprint 3.
- [ ] Main screen's universal search includes logical-agents; the `[logical-agents]` chip toggles visibility.
- [ ] Deleting a logical agent with active sessions shows a clear error from the API (don't silently cascade).

#### Test plan

- Unit: form-field coverage matching the `store.LogicalAgent` shape.
- Unit: main-screen row-union test ensuring `LogicalAgentRow` renders.
- Integration: against a daemon with seeded logical agents, run the full CRUD cycle.

#### Scope fences

- Do not implement hot/cold lifecycle transitions (warm ↔ cold) in this task — that's a v0.1 operational-agents concern. The TUI just edits the policy field.
- Do not implement memory-scope validation (existence of named scopes). That's Vanta's concern in v0.1.
- Do not couple LogicalAgent to any specific provider.
- Do not add tool-metadata rendering (tool descriptions, signatures). List strings only.

#### Relationship

Depends on: T-v003-s03-01, T-v003-s03-05 (form framework + validation), v002-s03 (storage shape), catalog-CRUD API decision.
Pairs with: T-v003-s05-02 (checkpoints are scoped per logical agent).

#### Origin

Context-pack [02-ideal-future-architecture.md](../agent-mux-vfuture-context-pack/02-ideal-future-architecture.md) entity model. Epic v0.0.3 exit criteria: LogicalAgent surface.

---

### T-v003-s05-02: Checkpoint list + detail + resume

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [feature, tui, checkpoint, resume]

#### Problem

v002-s05 exposes create/list/resume endpoints but the TUI has no way to drive them. For v0.0.3's checkpoint-resume milestone to feel real in the TUI, users need a clear "see checkpoints, resume one" surface.

#### Fix direction

- New package `internal/tui/runtime/checkpoints/`:
  - List screen scoped to a LogicalAgent: `GET /logical-agents/{id}/checkpoints`. Rows show short-id, created-at, summary snippet.
  - Detail screen: renders summarized context, pending work, key decisions — whatever payload shape the checkpoint ended up with. Pretty-print as labeled sections.
  - Resume action (`r`): posts `POST /logical-agents/{id}/resume` with `{checkpoint_id}`. In v0.0.2, the endpoint returns 501 — render that cleanly as "Resume lands in v0.0.3." In v0.0.3, surface the returned session ID.
  - Create action (`c` on a running session's detail screen in Sprint 2): already in scope via `POST /sessions/{id}/checkpoint`; capture the payload as a small inline form (summary, pending_work, key_decisions — all Multiline).
- Palette verbs: `view checkpoints`, `resume checkpoint`.
- Main-screen integration: `CheckpointRow` union member; filter chip.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/runtime/checkpoints/` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/client/checkpoints.go` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/detail/session.go` — bind `c` for checkpoint-create
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/row.go` — add `CheckpointRow`

#### Acceptance criteria

- [ ] List renders checkpoints per logical agent in descending `created_at` order.
- [ ] Detail screen pretty-prints all sections of the checkpoint payload.
- [ ] `r` posts resume; 501 responses are surfaced as a human message ("Resume lands in v0.0.3"), not a raw HTTP error.
- [ ] `c` on session detail opens an inline checkpoint-create form.
- [ ] Main screen's universal search includes checkpoints.

#### Test plan

- Unit: client tests for list / get / resume / create endpoints.
- Unit: 501-response rendering test.
- Integration (post-v0.0.3): real resume flow produces a new session.
- Manual: create a checkpoint from a running session; see it in the list; attempt resume; observe the v0.0.2 501 UX.

#### Scope fences

- Do not implement checkpoint payload schema validation in the TUI. The payload shape is intentionally loose in v0.0.2; render whatever comes back.
- Do not implement resume-with-overrides UX beyond the bare `{checkpoint_id}` payload in v0.0.3. Overrides are a v0.0.3 concern.
- Do not show checkpoint diff views.
- Do not delete checkpoints from the TUI in v0.0.3.

#### Relationship

Depends on: T-v003-s02-02 (session detail — `c` entry point), T-v003-s05-01 (scoped list per logical agent), v002-s05-03 (checkpoint endpoints), v0.0.3 Sprint 4 (resume).

#### Origin

Context-pack [02-ideal-future-architecture.md](../agent-mux-vfuture-context-pack/02-ideal-future-architecture.md) Checkpoint entity. [08-api-and-runtime-contract.md](../agent-mux-vfuture-context-pack/08-api-and-runtime-contract.md) checkpoint operations.

---

### T-v003-s05-03: BrokerEnvelope list + compose + reply

**kind:** agent
**priority:** 2
**manual:** true
**tags:** [feature, tui, broker, envelope]

#### Problem

v002-s05 exposes broker envelope CRUD with a persistence-only shape; v0.0.3 adds request/reply correlation. Without a TUI surface, envelopes are invisible to users and hard to debug.

#### Fix direction

- New package `internal/tui/runtime/broker/`:
  - List screen: rows show short-id, sender → recipient, correlation-id (truncated), created-at, one-line payload preview. Filters: `?recipient=`, `?workflow_id=`, `?correlation_id=`.
  - Detail screen: full envelope pretty-print, including the payload. Renders `correlation_id` as a clickable-like link that re-filters the list to the same correlation group (the "thread" view).
  - Compose screen: form with fields `sender`, `recipient`, `workflow_id` (optional), `payload` (multiline JSON). On submit, `client.PostBrokerEnvelope(...)`.
  - Reply action (`R` on detail): opens a compose form pre-populated with swapped sender/recipient and the target envelope's `correlation_id`.
- Palette verbs: `view envelopes`, `compose envelope`, `reply envelope`.
- Main-screen integration: `EnvelopeRow` + filter chip.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/runtime/broker/` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/client/broker.go` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/row.go` — add `EnvelopeRow`

#### Acceptance criteria

- [ ] List + detail render correctly with filters working.
- [ ] Compose form posts a new envelope and the list shows it on refresh.
- [ ] Reply pre-populates sender/recipient swap and `correlation_id` match; posting creates a second envelope correlated to the first.
- [ ] Thread view: from detail, hitting Enter on the correlation-id field filters the list to that correlation group.
- [ ] Payload multiline field accepts JSON; malformed JSON is surfaced as a validation error.

#### Test plan

- Unit: compose / reply serialization tests.
- Unit: JSON payload validation.
- Integration: post two correlated envelopes; verify the thread view surfaces both.
- Manual: compose from scratch, reply to an existing envelope, confirm round-trip.

#### Scope fences

- Do not implement request/reply waiting semantics here — the TUI is a write/read surface only. Semantic correlation is v0.0.3 Sprint 5.
- Do not implement push-to-session delivery.
- Do not auto-pretty-print unknown payload shapes. If it's JSON, pretty-print; otherwise render raw.
- Do not implement envelope deletion.

#### Relationship

Depends on: T-v003-s03-01, v002-s05-04 (broker endpoints), v0.0.3 Sprint 5 (correlation semantics).

#### Origin

Context-pack [02-ideal-future-architecture.md](../agent-mux-vfuture-context-pack/02-ideal-future-architecture.md) BrokerEnvelope. [08-api-and-runtime-contract.md](../agent-mux-vfuture-context-pack/08-api-and-runtime-contract.md) broker operations.

---

### T-v003-s05-04: Unified universal-search across all types

**kind:** agent
**priority:** 2
**manual:** true
**tags:** [feature, tui, universal-search]

#### Problem

Sprint 1's main screen mixes five types. This sprint adds three more. Without extending the search + filter-chip infrastructure, the new types would feel bolted-on.

#### Fix direction

- Extend `internal/tui/row.go` to include `LogicalAgentRow`, `CheckpointRow`, `EnvelopeRow` (populated by T-01 / T-02 / T-03 above).
- Extend the filter chip row to include the new types. Use short codes if width-constrained: `[proj] [agent] [prov] [launch] [sess] [lagent] [ckpt] [env]`. Assign new toggle keys: `6` logical-agents, `7` checkpoints, `8` envelopes.
- Fan-in loading: the Sprint 1 startup-load command fans out to eight parallel list calls now instead of five. Handle individual-type failures gracefully (if one list endpoint 500s, the others still populate).
- Type-badge styling in the row renderer uses a distinct theme color per type so the user can scan the list visually.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/row.go`
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/results.go`
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/main_screen.go`
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/theme/` — add per-type accent entries

#### Acceptance criteria

- [ ] Main screen renders all eight types in a single mixed list.
- [ ] Filter keys `1`–`8` each toggle the corresponding type.
- [ ] Partial-failure resilience: if one list endpoint errors, other types still render and a toast indicates the failed type.
- [ ] Per-type color badges make types distinguishable at a glance.
- [ ] Fuzzy search still works across the merged list.

#### Test plan

- Unit: fan-in test with a stub client where one endpoint errors; assert partial results + toast emission.
- Unit: filter-chip extension test (all 8 keys).
- Manual: verify readability on an 80x24 terminal (row density).

#### Scope fences

- Do not introduce saved searches.
- Do not introduce type-grouping view (all projects then all agents etc). Keep it flat-mixed.
- Do not support combined filters via palette (`filter type:launch project:demo`). Sprint 6+ if needed.
- Do not persist filter state across TUI restarts.

#### Relationship

Depends on: T-v003-s05-01, T-v003-s05-02, T-v003-s05-03.
Pairs with: T-v003-s06-05 (keybinding audit covers the new type-toggle keys).

#### Origin

User sketch: "Unified universal-search — mix projects/agents/providers/launches/sessions/logical-agents/checkpoints/envelopes."

---

### T-v003-s05-05: Event-stream side panel

**kind:** agent
**priority:** 3
**manual:** true
**tags:** [feature, tui, events, sse]

#### Problem

The daemon emits rich events via `GET /events/stream` (v002-s06). Without a TUI surface, those events are observable only via `curl`. A lightweight always-on side panel gives users situational awareness.

#### Fix direction

- New screen-overlay `internal/tui/events/panel.go`:
  - Toggleable via palette verb `show events` / `hide events` or keybinding `E`.
  - Renders as a right-side panel ~30 cols wide; the main viewport reflows to the remaining width.
  - Subscribes to `GET /events/stream` via the Sprint 2 SSE client pattern. Falls back to polling if SSE is unavailable.
  - Keeps the last N events (N=50 default) in a ring buffer.
  - Each event renders as: timestamp · scope · session-short-id · one-line message.
  - Filter: palette verb `filter events scope=session` or similar; Sprint 6 can refine. In Sprint 5, accept `scope` param only.
- Auto-hide after N minutes of inactivity (optional, off by default; config in Sprint 6 theme / settings).
- Do NOT auto-open. User must opt in per session.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/events/` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/client/events.go` (new; SSE subscription wrapper)
- `/Users/chrispian/Projects-apps/agent-mux/internal/tui/layout/` — optional right-panel composition

#### Acceptance criteria

- [ ] Palette verb `show events` opens the panel; `hide events` closes it.
- [ ] Panel subscribes to the daemon's event stream and renders events live.
- [ ] Ring buffer caps at 50 events; older events scroll out of view.
- [ ] Main screen remains usable with the panel open (layout reflows).
- [ ] Closing the panel cancels the SSE subscription cleanly.

#### Test plan

- Unit: ring-buffer eviction test.
- Unit: subscribe / cancel test against a stub SSE server.
- Manual: launch and stop sessions from another shell; verify events appear in the panel.

#### Scope fences

- Do not implement per-event drill-in (click to view full payload). A later sprint can.
- Do not persist the event buffer across TUI restarts.
- Do not add filtering sophistication beyond `scope` — leave that to Sprint 6 / v0.0.5+.
- Do not auto-open on errors or warnings.

#### Relationship

Depends on: v002-s05-05 (event stream endpoint), v002-s06 (event bus).
Pairs with: T-v003-s06-04 (toasts coexist with the panel).

#### Origin

User sketch: "Event stream side panel (optional toggle, `e` or palette `show events`) — subscribe to `GET /events/stream`. Keep it lightweight (last N events visible)."

## Review / readiness notes

- **Dependency on v0.0.3 semantics.** T-02 resume and T-03 reply correlation both gain real meaning in v0.0.3. In v0.0.3 the UX is present but the server may return 501 / skeletal results. Document this clearly in each screen (banner / toast on first view) so users understand why resume is a no-op against a v0.0.2 daemon.
- **LogicalAgent field shape.** The exact fields depend on v002-s03 landing the `logical_agents` table schema. If the shape differs from what T-01 anticipates, update the form field list; the form framework handles it.
- **Checkpoint payload pretty-print.** T-02 assumes checkpoints have `summary`, `pending_work`, `key_decisions` sections. The schema is loose in v0.0.2. Render whatever keys come back; the pretty-printer should be schema-agnostic with a "known keys get special styling, unknown keys fall through as generic sections" pattern.
- **Universal-search width.** T-04's 8 filter chips on an 80-col terminal is tight. Measure after T-04 lands; if ugly, compress to single-letter codes or group into a palette-driven toggle instead of chips.
- **SSE reconnect.** T-05's subscription should auto-reconnect if the daemon restarts. Trivial to add a backoff+retry wrapper; decide implementation detail at readiness.
- **Toggle key `E` vs `e`.** Sprint 3 uses `e` for edit; Sprint 5 T-05 uses `E` for events. Sprint 6's keybinding audit catches this.
- **Envelope payload editor.** T-03's compose uses Multiline for JSON. If JSON validation is a recurring complaint, consider a future "open in $EDITOR" escape hatch. Out of scope for v0.0.3.
