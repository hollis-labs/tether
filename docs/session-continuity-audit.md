# Session continuity caller audit (CW-20261115-0084)

Audited against Tether main `77464c6b` (2026-11-19), then completed by this
change. This is source/test evidence; deployment and real-bounce acceptance
remain separate work.

## What has landed since the original task

- PR #610 (`3d114340`): durable, server-produced same-worker native admission
  receipts. Authenticated item `GET /sessions/{id}` returns `native_admission`
  or null. A receipt is historical readiness evidence, not current liveness or
  authority to restart. Collection responses do not populate the detail.
- CW-20261118-0008, PR #611 (`0be3d1db`): stable opaque seat conversation IDs,
  generation history and store-only resolver. Session create/resume and team
  replacement move the conversation in the same transaction as the session.
  A keyed replay does not move it again. See `internal/store/conversations.go`,
  `internal/conversation/resolver.go`, and `docs/conversations.md`.
- CW-20261118-0010, PRs #612/#614 (`be6b1e21`, `34022ea3`): Claude PTY and
  streaming-stdio backends, observed runs, transcript/stream finalization.
- CW-20261118-0011, PR #616 (`55597672`): default-off `/chat/v1/`, stable
  conversation event log, generation reconciliation and stale-attachment
  refusal. `internal/chatwire/wire.go` installs `Store.OnConversationMoved`;
  `runner.go` signals `Replaced`; `control.go` reconciles against the store.
  This does not turn lineage-only create into a provider-native resume.
- MCP `tether_session_launch` already accepts `continuity=relaunch` as an
  assertion in `internal/mcpadapter/sessions.go:handleSessionLaunch`.
  The original task's claim that the argument is absent is stale.

## Callers and behavior

| Caller | Implementation | Continuity / refusal behavior | Tests |
| --- | --- | --- | --- |
| HTTP create, Agent Ops create | `internal/api/sessions.go:handleLaunch`, `internal/app/session_lifecycle.go:BuildLaunchPlan`, `internal/app/session_lineage.go:createContinued` | Explicit `continuity=relaunch` selects newest same logical agent and writes resume + parent; default/explicit fresh stays fresh. No earlier row is a first fresh start. Lookup failure now refuses, never silently drops lineage. | `internal/app/continuation_test.go` (fresh, relaunch, concurrency, creator/replay, failed lookup), `internal/api/principal_idempotency_test.go` |
| CLI launch | `cmd/tether/launch.go` | `--continuity relaunch` forwarded to HTTP create; fresh remains an explicit supported choice. `--replace` rebinds an ended idempotency key; it does not imply continuity. | `cmd/tether/launch_task_stamp_e2e_test.go` (authenticated client → real daemon adapter → store, bare create and relaunch), shared HTTP/create tests |
| MCP create then launch | `internal/mcpadapter/sessions.go:handleSessionCreate`, `handleSessionLaunch` | Create forwards continuity; native daemon views also preserve verified creator/key scope and honor explicit/catalog-default worker placement, refusing missing required or unavailable workers. Launch assertion rejects fresh/no-parent before launching. Neither infers native proof. | `internal/mcpadapter/session_continuity_test.go:TestSessionCreateContinuity`, `TestSessionLaunchAssertsContinuity`, `session_creator_test.go` |
| Legacy chat-send worker launch | `internal/api/chat.go:deliverChat` | Sends turn to idle running session; when creating, now always requests relaunch (first seat remains fresh). Unknown running health, unsettled nonterminal states and uncertain SendTurn errors refuse a new create. | `internal/api/chat_test.go:TestChatSendLaunchesWhenNoRunningSession`, `TestChatSendUncertainSessionNeverCreatesReplacement`, replay/delivery/refusal tests; `internal/app/worker_native_resume_test.go:TestWorkerCreateRelaunchPersistsCreator` |
| HTTP logical-agent resume | `internal/api/checkpoints.go:handleResumeLogicalAgent`, `internal/app/resume.go:resumeLogicalAgent` | Resolves exact native source where supported, preserves workroot/task/route and writes resume + parent. Native-only fences refuse before allocation on uncertain source, custody, authorization, native mapping or worker readiness; no generic create fallback. | `internal/app/codex_resume_test.go`, `native_resume_strict_coauthor_test.go`, `worker_native_resume_test.go`, `resume_route_test.go` |
| MCP logical-agent resume | `internal/mcpadapter/logical_agents.go:handleLogicalAgentResume` | Same resume service/API, native daemon view records the verified creator, no create fallback on error. | Shared resume tests, MCP logical-agent tests |
| Sysop logical-agent Resume | `apps/sysop/cmd/tether_sysop/main.go:handleLogicalAgentResume`, `internal/client/client.go:ResumeLogicalAgent` | Posts to logical-agent resume; errors return directly, no create fallback. | Shared HTTP/resume service tests; separate Sysop vet/test in `make check` |
| Boot / reboot auto recovery | `internal/app/boot_resume.go:bootResumeAgent` | Auto policy and pending work required; calls resume, not fresh create. Explicit native-only selection remains native-only. Worker sources and team members are excluded from ordinary boot resume. | `internal/app/boot_resume_test.go`, `native_resume_strict_coauthor_test.go` |
| Retained team recovery | `internal/app/team_session_resume.go:RecoverTeamSession`, `team_codex_replacement.go`, `team_session_replacement.go`, `internal/store/session_replacement.go:PrepareTeamReplacementTx` | Reattach positively retained live custody, otherwise replace only positively lost execution under original receipt/launch fences. Replacement writes resume + exact source parent and conversation generation atomically; retries read same destination. | `internal/app/team_session_replacement_test.go`, `team_codex_custody_test.go`, `team_session_resume_test.go` |
| Worker reconnect / flap | `internal/workerhub/hub.go:Connect`, `internal/workerruntime/replay.go:Restore` | Reconnect inventory/bindings and re-adopt surviving shims; no new session allocation or blind restart. Dead/corrupt custody remains fenced. `HTTPRelay.restartStandaloneStream` only reopens a stream. | `internal/workerruntime/restore_test.go`, `restore_corrupt_meta_test.go`, `restore_dead_test.go`, `resume_roundtrip_test.go`; workerhub tests |
| CLI boot profile | `cmd/tether/boot.go` | Explicit new profile boot, not a restart loop; fresh by default. | `cmd/tether/boot_test.go` |
| Session bootstrap | `internal/api/session_bootstrap.go` | Registers external identity only; supplied parent/intent are lineage, no runtime launch. Creator provenance is server-selected. | bootstrap and task-stamp tests |

Smoke scripts (`scripts/lifecycle-smoke.sh`) create independent test sessions;
their daemon restart checks retain the same session ID, not replacement creates.
MCP upstream process reconnects in `internal/mcpadapter/client_pool.go` restart
tool servers, not agent sessions.

Tachyon Restart/Continue and eager plan/history association are caller-owned
outside this repository and audited by the orchestrator. Agent Ops scripts
outside the repo must send `continuity=relaunch` on create; generic launch
cannot safely infer whether the operator intended a clean start.

## What “history carries” means

`intent=resume` plus `parent_session_id` is durable transcript lineage, not a
claim the provider's native thread was resumed. Tachyon can render inherited
history and move plans using that lineage under its own Continue/reset fences.
Tether native resume separately binds the provider mapping and preserved
workroot; native-only admission refuses rather than substituting recovery text.

For the new default-off chat wire, the seat's opaque conversation ID and event
log survive session/generation moves. `TestWireIntegration` in
`internal/chatwire/wire_test.go` covers two clients, generation advance, stale
attachments, reattach and replacement execution. A fresh provider session can
therefore retain UI event history without possessing the old provider context.
A deliberate chat reset opens a new chat conversation; a fresh session create
alone still moves the stable seat conversation by the already-landed #611 rule.

## Requesting principal

Migration 0109 adds nullable `sessions.requesting_principal`. Normal creates,
worker creates, resume and bootstrap persist the server-selected caller; keyed
replay leaves it unchanged. Boot recovery and retained replacement record their
internal origin. This is audit provenance, never a permission grant and never
copied from the source row. Historical rows stay NULL. The HTTP request cannot
choose it. The exact-row workspace/replacement snapshots include it so the new
column does not invalidate correctly admitted launches or weaken comparison.
Unknown creators are omitted from SessionRow JSON so migrated historical NULL
rows retain the byte layout used by existing workspace receipt hashes; known
creators remain included in newly earned receipt predicates.

`TestRequestingPrincipalMigration` applies all earlier migrations, inserts an
old session, upgrades twice, and verifies its unknown creator remains NULL.
Creation logging occurs after commit; logs contain principal IDs and lineage,
not raw credentials. See also `docs/trusted-session-context.md`.

Native MCP verified callers now use their own idempotency namespace rather than
sharing anonymous native keys. Existing anonymous records are not adopted or
relabeled: their historical creator cannot be verified retroactively.
