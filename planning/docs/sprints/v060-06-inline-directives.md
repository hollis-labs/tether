# Sprint v060-06 — Inline Directives

**Epic:** [v0.6 — Federation Directory](../epics/v0.6-federation-directory.md)
**Scope:** The `:<directive>` lane reserved by v060-05's symbol vocabulary, implemented as agent-side / session-runtime-side parsing in Tether. Ships a directives package with a small starter set (5-8 directives), `:help` for discoverability, strict line-start grammar, audit log, and the architectural seam for adding more directives later.
**Target duration:** ~6 business days.
**Transport/shape inheritance:** matches v060-01 / v060-05 conventions — UDS default, typed error envelope; ADR voice + length matching ADR 0036 / 0015.

**Consumers waiting on this:**
- Any agent doing repetitive idempotent ops (set timer, schedule self-update, mark read, mute, recall) that would benefit from shorthand over tool-call envelope.
- Audit consumers — every directive invocation gets logged with caller URN, directive name, args, result.
- The symmetric pair to FU-30 reflexes (system → agent injection). Directives are the agent → system injection lane.

**Coordination refs:** Symbol vocabulary locked in v060-05 D6 (`@`/`!`/`:`). This sprint preserves that vocabulary intact and implements the `:` half of it. No cross-substrate coordination needed pre-sprint; ship-notice goes to agridd-keeper at close so agridd can adopt the pattern in its own session layer (sibling implementation, not gated on this sprint).

**Dependencies:** v060-01 (Registry Foundation) for directive metadata registration semantics (optional — could ship without registry-backed metadata v1 by keeping directives compiled-in). v060-05 (Group Messaging) is NOT a prerequisite — `:` parsing is agent-side, daemon doesn't care. v060-05 ship is convenient because the symbol-vocabulary doc exists then, but not required.

---

## Exit criteria

- [ ] `internal/directives/` package lands with `Registry`, `Directive` interface, `Parser`, `Dispatcher`.
- [ ] Strict line-start grammar enforced: `^:([a-z_][a-z0-9_]*)(\s+(.*))?$`. Anything not matching is non-directive content (verbatim transport).
- [ ] Escape syntax `\:` transports verbatim (lifted from v060-05 D6 convention).
- [ ] Starter directive set landed (target 5-8): `:help`, `:set_timer`, `:schedule`, `:mark_read`, `:mute`, `:recall`, `:status`, `:cancel`. Final list locked in T-01 after a pre-design pass through agridd's high-frequency tool calls (FU-7 telemetry on `agent_known_tools.activation_count`).
- [ ] Each directive is **shorthand for an existing tool call** — no parallel implementation. The dispatcher resolves the directive to the underlying tool's execute path. (Acceptable for `:help` — that's introspection-only.)
- [ ] `:help` returns the registered directive list with name + 1-line description + example invocation, formatted for inline injection.
- [ ] Session-runtime interceptor wired into the message pipeline before storage + display. Configurable via a session-level flag (`directives_enabled: true` default, per-session opt-out).
- [ ] Directive results inject into the session as a system message (visible to next-turn LLM and to user). Format: `[DIRECTIVE :<name>] <result-summary>`.
- [ ] Audit log: every invocation writes a row to a new `directive_invocations` table with `(caller_urn, session_id, directive_name, raw_args, parsed_args_json, result_json, status, latency_ms, created_at)`.
- [ ] ADR `0016-inline-directives.md` captures: the symmetric-pair framing (agent → system, matching FU-30's system → agent), the no-parallel-impl rule, strict grammar, escape semantics, audit-log first-class.
- [ ] `make check` green.
- [ ] Cross-substrate `notice` sent to agridd-keeper announcing the package + the pattern (agridd is encouraged to adopt the same pattern in its session layer for cross-substrate consistency).

---

## Decisions locked

- **D1 Directives are shorthand for existing tool calls.** No new execution paths. `:set_timer */5 * * * * Check inbox` dispatches to `agent_schedule_add(spec=cron, value="*/5 * * * *", body="Check inbox")` internally. If a directive needs a tool that doesn't exist yet, write the tool first; the directive layer doesn't invent functionality.
- **D2 Line-start grammar only.** `:foo` at column 0 triggers; mid-line `:` is verbatim. Same convention v060-05 locked for all three symbols. Multi-line directives are NOT supported v1 (one directive per line).
- **D3 Escape syntax is `\:`** per v060-05 D6. Documentation, examples, etc. transport literally.
- **D4 Starter set is small + idempotent.** v1 ships 5-8 directives covering the highest-frequency idempotent ops. Risky ops (delete, deploy, state-change) stay in tool-call land where schemas gate. T-01 finalizes the list using agridd telemetry.
- **D5 Discoverability via `:help`.** Mirrors FU-7's `known_tools` pattern. `:help` returns the registered directives; `:help <name>` returns the per-directive doc + arg grammar.
- **D6 Results inject inline as system messages.** Visible to the next LLM turn (closes the loop — agent sees what happened) and to user-facing display. Format: `[DIRECTIVE :<name>] <result>`. Errors same format with `[DIRECTIVE :<name> ERROR] <reason>`.
- **D7 Audit log is non-optional.** Every invocation writes a row. The audit data is the foundation for the user's stated "auto-correct in later phases" goal — can't auto-correct without observation history.
- **D8 Session-level opt-out.** A session can disable directives via config (default enabled). Use case: untrusted session content where directive-injection-via-prompt-injection is a concern. v1 is a coarse switch; per-directive ACL deferred to a later sprint if needed.
- **D9 Registry-driven, not compiled.** The directive registry is initialized at session start from `internal/directives/builtin.go` v1; future sprints may extend with config-loaded directives. v1 keeps it compiled-in to avoid the "where do directives live" config sprawl.
- **D10 Per-directive argparser is the author's responsibility.** Each `Directive` impl owns its arg grammar (cron string, RFC3339 timestamp, URN, etc.). The framework provides helpers but doesn't enforce a uniform argparser — different directives need different shapes.
- **D11 No directive-emits-directive recursion v1.** A directive's result, when injected back into the session as a system message, must not contain `:` at line-start in a way that would re-trigger parsing. Either escape on inject or strip directive-shaped lines from results. v1 takes the simpler path: strip line-start `:` from result text before injection.
- **D12 Tether-side first; agridd adopts later.** This sprint implements directives in Tether's session pipeline. agridd may adopt the same pattern in its session layer (file an FU once the Tether implementation is proven). No cross-substrate coordination beyond the ship notice.

---

## Tasks

### T-v060-06-01: Directive starter-set finalization + ADR draft

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [directives, design, starter-set, adr]

#### Problem

Before writing parser/dispatcher code, lock the v1 directive list. Premature implementation leads to bikeshedding mid-sprint.

#### Fix direction

- Query agridd's `agent_known_tools` table (read-only) for `activation_count` rankings across all agents. Identify the top 15-20 most-frequently-called tools.
- Categorize by **idempotent vs side-effect-heavy** and by **structured-input vs flat-args**. Directive candidates: idempotent + flat-args.
- Propose starter set (5-8 directives) covering the highest-value candidates. Pre-thought list (refine in this task):
  - `:help` (introspection — no tool backing needed)
  - `:set_timer <cron-or-interval> <body>` → `agent_schedule_add`
  - `:schedule <cron> <body>` → `agent_schedule_add` (cron-only variant)
  - `:mark_read <message-id-or-all>` → `mux_message_mark_read`
  - `:mute <urn-or-group> [duration]` → (TBD — depends on whether a mute primitive exists; might defer)
  - `:recall <query>` → `memory_recall`
  - `:remember <namespace> <fact>` → `memory_write`
  - `:status` → introspection (active schedules + reflexes + last-tick summary)
  - `:cancel <schedule-id>` → schedule removal
- Draft ADR `0016-inline-directives.md` with the symmetric-pair framing (system→agent reflexes vs agent→system directives), no-parallel-impl rule, strict grammar, escape semantics, audit-log requirement.

#### Acceptance criteria

- [ ] Final starter list of 5-8 directives + their backing tools documented in the sprint file (update this doc with the result).
- [ ] ADR 0016 draft committed (final commit happens after T-08).
- [ ] At least one directive in the starter set is introspection-only (`:help`).

#### Scope fences

- No code in this task. Pure design + ADR draft.
- Don't add directives whose backing tools don't exist; either add the tool in a separate task OR drop the directive from v1.

---

### T-v060-06-02: Package skeleton + parser

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [directives, parser, package, foundation]
**depends_on:** T-v060-06-01

#### Fix direction

- New package `internal/directives/`:
  - `doc.go` — package overview, references ADR 0016 + v060-05 D6
  - `directive.go` — `type Directive interface { Name() string; Description() string; ArgGrammar() string; Execute(ctx, args string, caller CallerContext) (Result, error) }`
  - `registry.go` — `type Registry` with `Register(d Directive) error`, `Lookup(name string) (Directive, bool)`, `List() []Directive`
  - `parser.go` — `Parse(line string) (name, args string, isDirective bool)` enforcing the `^:([a-z_][a-z0-9_]*)(\s+(.*))?$` grammar at line-start (post-`\n` or buffer-start). Handles escape `\:` by stripping the backslash and treating as non-directive.
  - `result.go` — `type Result { Summary string; Payload any; Error error }` with formatters for inline injection (`[DIRECTIVE :<name>] <summary>` and `[DIRECTIVE :<name> ERROR] <reason>`).
- `CallerContext` carries `{ session_id, caller_urn, originating_message_id }` for audit + tool-dispatch downstream.

#### Acceptance criteria

- [ ] `Parse` correctly handles: bare directive (`:help`), directive + args (`:set_timer */5 * * * * foo`), escaped (`\:literal`), mid-line `:` (not a directive — `the time is :30`), no false positives in code blocks (test with multi-line buffer containing markdown code fence).
- [ ] Registry tests cover register, lookup, list, duplicate-register-rejection.
- [ ] `make test-race` green.

#### Scope fences

- No directive implementations in this task. Just parser + registry skeleton.
- No session-pipeline integration. That's T-04.

---

### T-v060-06-03: Starter-set directive implementations

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [directives, builtin, starter-set]
**depends_on:** T-v060-06-02

#### Fix direction

- New `internal/directives/builtin.go` registering the v1 starter set. Each directive is one struct implementing `Directive`:
  - `helpDirective` — looks up registered list from the registry, formats the response. Special-case `:help <name>` returns per-directive grammar + description.
  - `setTimerDirective` — parses `<cron-or-interval> <body>`; dispatches to `agent_schedule_add` tool via the existing tool-execute path.
  - `scheduleDirective` — cron-only variant; same backing tool.
  - `markReadDirective` — parses `<message-id-or-all>`; dispatches to `mux_message_mark_read` (or iterates for `all`).
  - `recallDirective` — parses `<query>`; dispatches to `memory_recall`.
  - `rememberDirective` — parses `<namespace> <fact>`; dispatches to `memory_write`.
  - `statusDirective` — introspection-only, queries session state + active schedules + recent ticks; no backing tool.
  - `cancelDirective` — parses `<schedule-id>`; dispatches to schedule removal (whichever existing tool covers this — verify in T-01).
- Each directive: minimal arg validation, clear error responses on bad input.
- `Init(reg *Registry)` registers all builtins.

#### Acceptance criteria

- [ ] Each directive callable via the registry; happy path produces correct `Result`.
- [ ] Bad args produce useful error messages (not panics or generic "internal error").
- [ ] Directives that back tool calls do NOT replicate tool logic — they call the tool's execute path.
- [ ] Tests cover happy path + at least one error per directive.

---

### T-v060-06-04: Session-pipeline interceptor + audit log

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [directives, session, pipeline, audit]
**depends_on:** T-v060-06-03

#### Fix direction

- Find the existing session output pipeline in `internal/session/` (where LLM output is collected/post-processed before storage + display). Add a `DirectiveInterceptor` hook that runs before storage.
- The interceptor:
  1. Scans the output text line-by-line for the directive grammar.
  2. For each match: calls `Registry.Lookup(name)`, executes if found.
  3. On execute success: removes the directive line from output and injects `[DIRECTIVE :<name>] <result.summary>` as a system message into the next-tick context. (Configurable: leave-in-place vs strip-on-success.)
  4. On execute error: replaces the directive line with `[DIRECTIVE :<name> ERROR] <reason>`.
  5. On no match: leaves the line verbatim (it's not a directive, just content starting with `:`).
- Writes one row per invocation (including no-matches at debug-level) to `directive_invocations` table.
- New migration `internal/store/migrations/0018_directive_invocations.sql`:
  ```sql
  CREATE TABLE directive_invocations (
    id              TEXT PRIMARY KEY,
    session_id      TEXT NOT NULL,
    caller_urn      TEXT NOT NULL,
    directive_name  TEXT NOT NULL,
    raw_args        TEXT,
    parsed_args_json TEXT,
    result_json     TEXT,
    status          TEXT NOT NULL CHECK(status IN ('ok','error','no_match')),
    latency_ms      INTEGER,
    created_at      DATETIME NOT NULL
  );
  CREATE INDEX idx_directive_inv_by_session ON directive_invocations(session_id, created_at);
  CREATE INDEX idx_directive_inv_by_name ON directive_invocations(directive_name, created_at);
  ```
- Session config flag: `directives_enabled` (default `true`). Per-session opt-out gates the interceptor.

#### Acceptance criteria

- [ ] Send a session message containing `:help` → interceptor fires, audit row written, result injected as system message.
- [ ] Send a message with `\:not-a-directive` → no interception (verbatim transport).
- [ ] Send a message with mid-line `:colon` → no interception.
- [ ] Send `:unknown_directive foo` → audit row written with status `no_match`; line stays verbatim (or replaced with error per impl decision — pick during impl).
- [ ] Send `:set_timer bad-cron foo` → audit row written with status `error`; error message replaces the directive line.
- [ ] Session with `directives_enabled=false` → interceptor skipped, all lines transport verbatim.
- [ ] `make test-race` green.

#### Scope fences

- No HTTP / MCP / CLI surface for directives v1 (you invoke them by writing in a session message, not via a separate API).
- No per-directive ACL or role check v1. Coarse session-level switch only.

---

### T-v060-06-05: CLI introspection — `mux directives ...`

**kind:** agent
**priority:** 2
**manual:** true
**tags:** [directives, cli, introspection]
**depends_on:** T-v060-06-04

#### Fix direction

- New `cmd/mux/directives.go`. Subcommands:
  - `mux directives list` — registered directives with name + description.
  - `mux directives show <name>` — per-directive doc + arg grammar + recent-invocation count.
  - `mux directives audit [--session <id>] [--name <name>] [--limit N]` — recent invocation history from `directive_invocations`.
- Output: pretty (default) + `--json` (machine).

#### Acceptance criteria

- [ ] All three subcommands working against a fixture daemon.
- [ ] `audit` correctly filters by session + name.

#### Scope fences

- No invoke-from-CLI (`mux directives invoke :foo args`) v1 — directives are session-context primitives, not standalone CLI commands. Users wanting CLI-driven invocation use the underlying tool directly.

---

### T-v060-06-06: Symbol vocabulary doc + directives docs

**kind:** agent
**priority:** 2
**manual:** true
**tags:** [directives, docs]
**depends_on:** T-v060-06-04

#### Fix direction

- Extend `docs/groups/symbols.md` (created in v060-05 T-07) with a forward-link to the directives reference docs.
- New `docs/directives/`:
  - `overview.md` — what directives are, the symmetric-pair framing (vs reflexes), discoverability via `:help`.
  - `starter-set.md` — full reference for each v1 directive: name, grammar, args, examples, error cases.
  - `authoring.md` — how to add a new directive (for future expansion).
- Extend `docs/api/README.md` with `## Directives` section pointing at the dedicated docs.

#### Acceptance criteria

- [ ] All three new doc files landed.
- [ ] At least one worked example per directive in `starter-set.md`.

---

### T-v060-06-07: ADR 0016 finalization + cross-link

**kind:** agent
**priority:** 2
**manual:** true
**tags:** [directives, adr]
**depends_on:** T-v060-06-04, T-v060-06-06

#### Fix direction

- Finalize `docs/adr/0016-inline-directives.md` (draft started in T-01).
- Cross-link from v060-05's ADR 0015 (symbol vocabulary) — add a "see also" pointer to ADR 0016 in the `:` section.
- Verify ADR number is actually 0016 (sprint plan name is suggestive; if 0016 was claimed since, pick the next free number and update this doc accordingly).

#### Acceptance criteria

- [ ] ADR 0016 landed, dated, linked from API + directives + symbols docs.
- [ ] Cross-link in ADR 0015 added.

---

### T-v060-06-08: Cross-substrate ship notice + dogfood self-test

**kind:** agent
**priority:** 3
**manual:** true
**tags:** [directives, ship-notice, dogfood]
**depends_on:** T-v060-06-04, T-v060-06-05, T-v060-06-06, T-v060-06-07

#### Fix direction

- Dogfood self-test before sending the ship notice:
  - Open a Tether session.
  - Issue each directive in the starter set at least once.
  - Verify each: produces the expected `Result`, injects the system message correctly, writes the audit row.
  - Issue one intentionally-malformed directive per kind (bad cron, unknown name) and verify the error path.
- Ship notice from `msg://agent/agent-mux/tether-registry-design` to `msg://agent/agent-mux/agridd-keeper`:
  - Subject: `Sprint v060-06 SHIPPED — inline directives live + pattern available for agridd adoption`
  - Body: announce the package, the starter set, the `:help` discoverability, the symmetric-pair framing with FU-30 reflexes, link to the docs + ADR. Encourage agridd to adopt the pattern in its session layer (file an FU when ready).

#### Acceptance criteria

- [ ] Self-test results captured in sprint close notes (which directive produced what output).
- [ ] Ship notice sent; message_id recorded.

---

## Review / readiness notes

- **Pre-design hinges on agridd telemetry.** T-01 reads `agent_known_tools.activation_count` to ground the starter set in actual usage. If agridd's data isn't representative (it's mostly the Supervisor), supplement with intuition + the curated tool sets from FU-7's role_seed assignments.
- **The "no parallel impl" rule is load-bearing.** If a directive needs to do something the underlying tool can't, write the tool first. Directives that diverge from their backing tools create maintenance debt fast.
- **Audit log doubles as auto-correct foundation.** The user explicitly flagged "auto-correct in later phases" as a goal. The audit table makes that possible — without observation history, there's nothing to learn from.
- **Per-directive arg grammar is the hardest part.** Be careful with cron expressions (`*/5 * * * *`), free-form bodies, and URNs (which can contain `:` themselves — escape rules matter).
- **No directive-emits-directive recursion v1** (D11). Result text is sanitized before injection. If a future sprint wants directive composition (`:macro do_a_thing` that runs three directives), that's a separate feature with its own threat model.
- **Pattern portability.** The session-pipeline interceptor pattern is what agridd needs to land for its own directive support (FU once Tether ships). Document the integration shape clearly in `docs/directives/authoring.md` so other substrates can adopt without re-deriving.
- **No directive ACL v1.** Coarse opt-out per session. Per-directive role gating (e.g. "only owner can `:cancel`") is a follow-up if directive misuse becomes a real problem.

---

## Done checklist (at sprint close)

- [ ] All eight task acceptance sections ticked.
- [ ] Exit criteria above all ticked.
- [ ] `make check` green.
- [ ] ADR 0016 (or next free) committed.
- [ ] Branch FF-merged to `main`, branch deleted.
- [ ] Ship notice sent to agridd-keeper; message_id recorded.
- [ ] Self-test results captured.

---

## Hand-off snippet for the parallel agent

> You're executing Sprint v060-06 — Inline Directives. Scope is the `:` lane reserved by v060-05's symbol vocabulary: a directives package in Tether that parses `:<directive>` at line-start, dispatches each to its backing tool via the existing tool-execute path (no parallel implementation), and injects results inline as system messages. Read the epic at `planning/docs/epics/v0.6-federation-directory.md`, v060-05's D6 detail (for the symbol vocabulary), and this sprint file end-to-end before starting. The "no parallel implementation" rule (D1) is load-bearing — directives are SHORTHAND, never replacements. Starter set is 5-8 directives, all idempotent, finalized in T-01 against agridd telemetry. Audit log (D7) is non-optional. `make check` green is non-negotiable. Ship notice to agridd is the last step.
