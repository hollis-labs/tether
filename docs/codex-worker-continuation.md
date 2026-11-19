# Same-worker Codex native continuation (CW-20261116-0015)

This patch was initially reviewed on installed Tether b86f7292 and is now reconciled with main 0d0ac59c (CHAT2/PR604 and prerequisite PR608), including the root and Sysop dependency updates. It requires matching hub and worker builds, the worker feature `codex-native-resume-v1`, and `catalog.defaults.worker_resume`. It does not enable that setting, change a catalog, move a live manager, or implement Tachyon thread association. Independent exact-head review and operator-approved installation/transition remain required.

## Contract

`POST /logical-agents/{id}/resume` accepts `native_only:true`, exact `source_session_id`, `worker_source_generation`, and `worker_policy_digest`. Ordinary Claude resume retains its existing behavior. Codex worker continuation does not accept a workroot override, a provider switch, a fresh-context fallback, or an automatic boot turn.

The source must be the latest terminal attempt, except an unadmitted failed native-only child may be retried from its explicitly named unchanged source. A mapped, successful, live, unrelated, or uncertain child cannot authorize that retry. The same connected worker supplies a content-free source receipt: canonical source id, shim boot generation, actor, actual worker workroot and a single unambiguous native thread id extracted from its retained output. Missing/pruned/oversized/ambiguous evidence fails closed. A live provider child blocks admission; an exited provider's retained journal shim is not a writer. Filesystem paths must remain canonical and under the worker workspace root.

The policy digest is the current catalog-resolved SessionPolicy sealed using the **source session id**, so it is reviewable before a child UUID exists. Every admission re-resolves current catalog authority. The caller must hold independently verified `session.write` and `catalog.write` (or wildcard) in an exact still-live stored credential; JWT-only authority is not supported by this bounded path. Source/current credential scope and expiry ceilings still constrain the new session and issued-token grant. The old session_mcp_policy row is read for provenance and never edited. The new child receives its own sealed snapshot; a missing/stale policy digest is refused before allocation. This does not itself approve the upstream reader binding or confinement opt-in.

On the worker, exclusive durable source-generation reservation precedes native-state copying. It prevents concurrent and replayed forks, including across worker restart. New provider artifacts and credentials are planted separately. Only native history is copied; the source home, source credential configuration, source policy and actual workroot are not rewritten. The app-server receives `initialize`, `initialized`, then `thread/resume` with the exact thread and cwd. Its returned id must match before readiness is published. There is no `thread/start` or `turn/start` during admission. Subsequent explicit user turns use that bound native thread.

## Activation gate and sequence

1. Independent review must approve the exact combined hub/worker commit and tests, including the main and CHAT2 dependency delta. The installed b86f7292 catalog-content-bytes fix (CW-20261115-0127) is preserved exactly in separate commit 72f5fe74, without unrelated PR602 changes. Review the additional main and migration inventory below before installation.
2. AOM obtains explicit one-manager-at-a-time transition approval and the separately named reader-origin/catalog approval, including any `allow_unconfined_remote` opt-in. Capture canonical URN, current session, real worker workroot/native mapping, source generation, old saved policy digest, chat/history/thread and plan id/revision. Back up using the managed backup path. No credential values belong in evidence.
3. Verify the installed worker advertises the feature, remains connected, and retains the source execution/native files. Missing historical evidence is a refusal, not permission to guess or repair DB rows.
4. After the authorized controlled stop and confirmed provider retirement, use the authenticated local UDS resume API with the exact source and `native_only:true`. A request without the policy digest returns the proposed current digest before allocating any child. Review the named current catalog policy before supplying that digest. With an approved digest and missing generation, the source probe reports its observed generation, again before allocation. Remote error bodies deliberately do not reveal these internal details; use the supported local executor, not a borrowed credential. Re-read current state if anything changes between preparation and acceptance.
5. Submit the exact reviewed digest and generation with an idempotency key. Verify new UUID and explicit parent, unchanged canonical identity, exact worker/workroot/thread, new sealed policy and old unchanged policy. Perform the actual authorized reader calls. Neither source lineage nor a successful fake-provider test substitutes for these receipts.
6. Verify Tachyon association and owner chat roundtrip before moving the second manager.

## Failure and rollback

A hub admission refusal before any worker launch frame is sent settles the allocated child as failed, allowing an exact-source retry after reconnect or renewed authority; the retry repeats all current authority and source checks. Before provider start, a rejected worker attempt releases its source reservation. After spawning, the reservation remains until retirement is positively confirmed. A native-readiness failure asks the shim to stop and waits for a terminal/dead-child receipt. Only that confirmed failure releases the reservation; unknown teardown retains it and refuses further writers. A later retry may reclaim it under the worker admission lock only after the retained attempt proves terminal, its child is gone, and no native-admission receipt was published. The hub retains the failed/uncertain child and historical source. A failed child without native custody can be retried against the original exact source, generation and policy; it still must pass the worker reservation. Never delete the claim or launch fresh to bypass a refusal.

A failed or ambiguous response is not permission to stop another session, reuse a credential, mutate a saved policy, or relaunch blindly. Reconcile the recorded attempt through its lifecycle owner. Missing/pruned source state requires an explicit owner decision outside this native-continuation contract. Once a new thread writer has run, restoring an old executable/catalog does not rewind conversation state: preserve both attempts and have AOM select the supported recovery path. Installation rollback is the prior known artifact through Cerberus, separately approved; catalog rollback uses its exact sanctioned backup. Do not mix either with history deletion.

## Tachyon dependency and integration acceptance

Tachyon must associate the replacement with the existing owner-visible thread/history and original plan immediately at continuation completion, before the agent makes any proxied tool call. The old lazy plan `Service.Attach` behavior is insufficient (0084/0128). Required fields are old/new canonical session UUIDs, stable agent identity, verified parent, same worker, actual native thread and workroot. Tachyon owns the UI association delta; CHAT-1 consumes the recovery contract. Source messages, plan identity, steps and revision history must survive, without duplicating a plan.

Integration acceptance is a real authorized one-manager transition: before/after native-thread/workroot/URN and saved-policy evidence, immediate panel plan/history before any tool call, then a user question depending on pre-transition context and real reader calls. Parent0084 and access0003 retain this live acceptance; this patch does not close them.

## Deployment delta inventory

The comparison is installed `b86f7292aa7883cec1851e85fc1cbeccfc0841a7` to main `0d0ac59c7df9cad1f2c803655b69e7c3a1e76824`, plus this continuation series and the exact catalog preservation commit. The installed catalog commit is not an ancestor of main; their common baseline is `673598263d1b3c4036b7c72d4ca403eb42ae35fc`. This is source inventory only: no live database was inspected or migrated.

| Main change | Deployment review concern |
| --- | --- |
| PR599 / 775e6e58 | Launch continuation assertions and principal audit recording. |
| PR594 / 52deeca6 | A seat's own live session can set its own status. |
| PR592 / bfa4d72d; PR608 / 28cc708b | Migration progress and status/apply CLI; corrected pre-migration test fixture. |
| PR586 / 62083396 | Operator-attested replies, catalog trust-root configuration and approval, new migration0106. Installing code does not authorize trust-root enrollment. |
| PR598 / 783acbcd; PR589 / 91f1678c | Shimhost/OpenAI/Codex test stability and test-rig shutdown changes. |
| PR581 / 666abaee | MCP HTTP upstream retry/backoff and daemon upstream-state reporting. |
| PR584 / 6840a09a | Session-log group commit, per-session ingest locking and worker ingestion batching. |
| PR606 / 51a4fb51; PR609 / 1b6ffaa6 | Reply wakes, coalescing, turn-end rearming and pending-mail seat digest wakes. |
| PR605 / 719ddb00 | Canonical tool-call logging and bounded WAL maintenance. |
| PR607 / 0c45cc10 | Request envelope kind and typed ask/reply tools. |
| PR604 / 0d0ac59c | Opt-in terminal attach and writer leases over chathost. Root and Sysop both move mesh/core to v0.11.0, agent/core to v0.5.0 and agent/sdk to v0.0.0-20261117000959-bc1013d0655e. |

The only added numbered SQL migration in that comparison is `internal/store/migrations/0106_operator_attested_replies.sql`. It creates `operator_reply_challenges` plus its device/consumption/expiry index, `operator_reply_proofs` with unique proof/elevation replay guards, `operator_reply_answers` keyed by parent message, and singleton `operator_attestation_trust`; it adds nullable `detail TEXT` to `identity_audit`. It does not populate a trust root or rewrite session/plan history. The continuation patch adds no numbered migration.

At the separately authorized deployment gate, the release owner must obtain a managed database backup and inspect pending migrations using the candidate's read-only `tether migrations status --state-db <approved-path> --json`. Actual pending versions depend on the live database and are not established by this inventory. `tether migrations apply` writes and must run with the daemon stopped, per its command contract; ordinary store opening also migrates. Neither command was run against live data here. Migration locking, progress reporting and the added audit column must be included in the maintenance plan. There is no down-migration in this patch: binary rollback alone does not reverse schema, proofs, or conversation writes. Any database restore requires its own approved recovery plan and accounting for writes since backup.

## Native admission read contract

Authenticated `GET /sessions/{replacement_session_id}` returns `native_admission: null` for historical, fresh, refused or unconfirmed attempts. A non-null object is a durable hub observation written only after the connected worker reports exact Codex native readiness:

```json
{"kind":"codex_same_worker_v1","source_session_id":"source UUID","source_generation":"source shim generation","replacement_session_id":"replacement UUID","replacement_generation":"replacement shim generation","worker_id":"worker ID","logical_agent_id":"canonical logical agent ID","native_thread_id":"provider thread ID","workroot":"verified actual workroot","admitted_at":"RFC3339Nano UTC"}
```

The authenticated remote projection replaces `workroot` with an opaque `tether-ref:workroot:` reference; it does not disclose the host path. The local authenticated endpoint retains the exact path. The receipt is historical evidence, not current liveness or permission to launch another writer. Consumers must also verify the expected source/replacement identity and current state. The collection endpoint does not populate this detail: use the item GET.

The hub stores the receipt once in a server-only member of the existing `launch_plans.plan_json`; it is absent from the typed launch plan and request models, so copies and caller-supplied fields cannot manufacture it. No schema migration is added. Readiness validates replacement session/generation, actor, native thread and workroot against the authorized launch; the receipt binds the earlier checked source generation as well. Missing or conflicting evidence fails closed. Lost launch responses may leave a real worker running without a hub receipt; these remain unconfirmed and require reconciliation rather than inference from intent, state or provider mapping.

The bounded UI acceptance path is AOM-authorized continuation followed by Tachyon association. A UI session-control client without the explicit source generation, current policy digest and independent current `catalog.write` authority must refuse preserve-context worker Restart before stopping or changing a session. This patch does not introduce a delegated approval flow or widen authority; full UI restart parity remains separate work under0084.
