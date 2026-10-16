# Identity enforce rollout runbook (CW-20261010-0268)

Phase 3 of CW-20260930-0253. Feeds the policy enforcement flip CW-20261010-0032.
Background: [caller-identity.md](caller-identity.md). **This runbook performs no
flip by itself.** Every flip needs a separate owner GO per enforcement point.

## 1. The switch

Identity mode is configured in the catalog's `global.yaml` and is read **once, at
daemon start**. There is no live reload: a flip or rollback takes effect on the
next `tether daemon` restart.

```yaml
identity:
  mode: observe          # global default: off | observe | enforce
  points:                # optional per-point override; omitted point follows mode
    mcp_gateway: enforce
```

Points with their own override today: `local_api` (local listener auth
middleware), `mcp_gateway` (daemon MCP transport, `/mcp` and `/p/<profile>`),
`teams` (team principal resolution). Everything else follows the global
`identity.mode` (see the fail-mode table), including session-token minting
(global mode only: a point flip changes no token behavior). Startup bind rules use
the per-point modes, see "Bind rules per point" below.

Commands (all take `--catalog`, default `~/.tether/catalog`):

```bash
tether identity mode                         # configured vs RUNNING mode per point, restart-pending, flip history, chain check
tether identity flip --point mcp_gateway --to enforce --evidence "CW-… GO rec <id>"
tether identity flip --global --to enforce --evidence "…"
tether identity rollback                     # undo every pending flip, restoring prior value AND absence of overrides
tether identity rollback --point mcp_gateway --evidence "why"
tether identity rollback --observe           # break-glass: explicit observe on global + every point, ignoring history
tether daemon stop && tether daemon start    # apply
tether doctor                                # caller-identity: effective per-point modes, restart-pending, flip-log integrity
```

`rollback` replays the flip log and reverts, newest first, flips not yet rolled
back, restoring each target's prior configured value; an override that did not
exist before the flip is removed again (the point follows global), and targets
with nothing pending are untouched. It cannot weaken a point that was enforcing
before the log began; use `--observe` for an emergency, knowing it does exactly that.

`--evidence` is mandatory on a flip. Each change appends one JSON line to
`<state root>/run/identity-flips.jsonl` (0600): `{at, action (flip|rollback|reset),
by, point, from, to, prior, evidence, catalog, prev, hash}`. `by` defaults to the OS
user and is an **unauthenticated label**. The log is a **hash chain**: `prev` is
the previous record's `hash`, `hash` is SHA-256 over the record's other fields.
`identity mode`, `rollback`, further flips and `tether doctor` verify it; any
malformed, blank, altered, removed, reordered or inserted record is reported
loudly (doctor `fail`, `identity mode` exits non-zero) and further flips/rollbacks
refuse until the log is restored. The audit line is written before global.yaml is replaced
(atomic rename, comments preserved), so no change is unrecorded; if the write
then fails, `identity mode` shows configured values disagreeing with the last record.
Back up `global.yaml` and the state DB before the first flip (operator token and DB stay a matching pair).

**Limits of the chain.** It detects edits, deletions in the middle, reordering and
forged lines made without recomputing the chain, but the owning uid can rewrite
the whole file consistently, or truncate the tail (a shorter log still verifies).
Pin the **head hash** printed by `tether identity mode` in the GO record and in
Tesseract after every flip, and compare it at the next review; a head that is not
in your pinned history means the log was rewritten.

### Operator boundary (precondition)

The verbs have no authentication: they run as the invoking uid and edit
`global.yaml`. They refuse to run when `TETHER_MCP_TOKEN` or `TETHER_TOKEN` is set
(the markers Tether plants in every launched session; an operator who exports a
token for CLI use must unset it for these verbs), which stops a launched session
from flipping by accident. **That is a guard, not a boundary**: a process can unset
its environment. Today the catalog, `run/` and `state/` are write-protected only for
agents Tether wraps (Claude, OpenCode), **not Codex (CW-20261001-0230)** and not
against processes outside Tether, and `run/` including `operator.token` is
**readable by agents** until read protection CW-20261001-0263 lands. A same-uid
process with catalog write access can therefore edit `identity` in `global.yaml` or the
flip log directly and disable enforcement at the next restart. Enforcement
integrity against Codex or any other same-uid process depends on read/write
protection landing; do not describe a flip as tamper-resistant before then. Mitigations
until then: review `tether identity mode` (running vs configured, chain) after each
restart, and keep the pinned head hash.

### Bind rules per point

The local listener's bind is validated against the **effective mode of the points served
on it**: `local_api`, plus `mcp_gateway` when the MCP endpoint is mounted. A
non-loopback TCP bind starts only if each of them is `enforce`; a global enforce
does not mask a point left at observe or off. Identity storage and operator
bootstrap run whenever any point or the global mode is not off, and use the
strictest of them (a mismatch refuses startup if anything enforces).
Setting the global mode to `off` while a point enforces is a footgun: no session
tokens are minted, so every session is rejected at that point.

## 2. Observe-mode review

Schema: `event` (migration 0066) is empty for request rows and names a finding
otherwise; `verified` (0068) is 1 when a bearer resolved to a principal and is **not
backfilled**: rows from before 0068 default to 0 whatever their `authentication`.
So the queries key on `authentication = 'verified'` (valid for all history) and
`event = ''`; use `verified = 1` only for windows entirely after 0068. On a database
without 0066/0068 columns, drop the `event` predicates. `route_pattern`,
`required_scope`, `would_allow` come from 0065 and are NULL/empty on rows from before it.

Run on a copy or read-only: `sqlite3 -readonly <state_db> '…'` (`defaults.state_db`
in global.yaml). Window = the whole observe period, minimum **7 days of normal
use including at least one full launch→message→close cycle per agent provider in use**.
`at` is UTC ISO-8601 text.

**Blind spot:** anonymous requests (no `Authorization` header) are *not*
audited. Their volume is visible only as the 401 count after a flip, so find
anonymous callers by relaunch inventory (section 4), not by this table. Rows exist only for
requests that carried a credential.

```sql
-- 1. authentication outcome mix. Threshold: invalid+unavailable = 0 in the last 3 days
SELECT authentication, count(*) FROM identity_audit WHERE at > :since GROUP BY 1;

-- 2. who calls what; every route family used by a verified principal
SELECT principal_id, route, method, count(*) n FROM identity_audit
 WHERE authentication='verified' AND at > :since GROUP BY 1,2,3 ORDER BY n DESC;

-- 3. would_allow = 0 review (scope table, enforce-to-be). Every row must be explained.
SELECT principal_id, method, route_pattern, required_scope, count(*) n
  FROM identity_audit WHERE event = '' AND would_allow = 0 AND at > :since
 GROUP BY 1,2,3,4 ORDER BY n DESC;

-- 4. would_allow IS NULL on REQUEST rows: verified request on a route/method with no table entry.
--    event <> '' rows (from_unverified, from_refused, …; internal/api/sender_identity.go) are
--    handler findings whose scope fields are NULL by design, so they are excluded here.
SELECT method, route, count(*) FROM identity_audit
 WHERE event = '' AND authentication='verified' AND would_allow IS NULL AND at > :since GROUP BY 1,2;

-- 4b. handler findings (I2 sender/as verification), informational: expect 0 *_refused before enforce
SELECT event, principal_id, count(*) FROM identity_audit WHERE event <> '' AND at > :since GROUP BY 1,2;

-- 5. verified request rows per principal kind
SELECT p.kind, count(*) FROM identity_audit a
  JOIN (SELECT DISTINCT principal_id, kind FROM principals) p ON p.principal_id=a.principal_id
 WHERE a.event = '' AND a.authentication = 'verified' AND a.at > :since GROUP BY 1;
```

Thresholds to flip an enforcement point (all must hold; record the numbers in the GO record):

1. Query 1: zero `invalid` and zero `unavailable` over the last 72 h; `/health`
   `identity.audit.dropped` and `failures` both 0 (doctor warns otherwise).
2. Query 3: `would_allow = 0` is **expected to be non-zero** for session
   principals until I4 lands (they hold `session.write`-style scopes, the table
   wants more). Each distinct (principal kind, route_pattern, scope) group must be
   either fixed (table or grant corrected) or listed in the GO record as an
   accepted denial with the owner's initials. For a point whose enforcement
   rejects on scope, the unexplained count must be 0; for authentication-only
   enforcement (all three points today: 401/503 on credential, no scope
   rejection) the review is informational but still must be done, since the
   scope gate follows.
3. Query 4: no route used by a verified principal is missing from the table
   (a structural test also guards new routes).
4. Operator custody: doctor shows `observe; operator credentials available`.
5. Relaunch inventory (section 4) lists no live session the point needs that
   is still tokenless.

## 3. Pre-flip checklist (minimum, owner ruling 2026-10-10)

| Item | Task | Evidence to attach |
|---|---|---|
| I1 local route-to-scope table | CW-20261010-0261 (merged) | structural test green; query 3/4 output |
| I2 server-stamped `from` | CW-20261010-0262 | merged commit; test names |
| I3 per-principal idempotency | CW-20261010-0263 | merged commit; test names |
| I4 reply/delivery authorization incl. explicit turn-router authorization | CW-20261010-0266 | merged commit; test names |
| Read protection against same-uid agents | CW-20261001-0263 | `tether doctor` sandbox-protect rows for the providers in use |
| I5 verified flag | CW-20261010-0264 | not in the owner's minimum; note state |
| Backup | — | state DB + `run/operator.token` copied together |
| Observe review | section 2 | query outputs, thresholds met |
| Relaunch plan | section 4 | list of sessions to relaunch |
| Rollback rehearsed | section 1 | dry-run transcript (see below) or a rerun |
| Operator boundary understood | section 1 | note whether Codex/same-uid agents run; read/write protection state |
| Flip-log head hash pinned | section 1 | `tether identity mode` head hash |

Budgets (CW-20261001-0607) and delegation are not preconditions. Observe-mode
attribution alone does not stop same-uid credential theft: without read
protection, an agent can read another session's token from its boot files.

## 4. Relaunch of sessions started before the switch

A session launched when the global mode was `off`, or before identity existed,
holds **no token** (`TETHER_TOKEN` is absent), as do observe-mode launches whose
mint failed (they continue anonymously). Under an enforcing point those sessions are 401.
Sessions launched in observe hold a 7-day token and keep working.

1. Before the flip: `tether sessions` — list running sessions; launches before the
   identity rollout (or in `off`) are the relaunch set. Note logical agents.
2. Flip the point; restart the daemon. (The restart itself ends the daemon's
   control of running session handles; follow the normal restart/resume procedure.)
3. For each tokenless session: stop it and launch a replacement (a resumed session
   gets a new session id and a fresh token). Native-only resume while global
   mode is `enforce` follows the stricter path in `session_lifecycle.go`.
4. Verify: `identity_audit` shows `verified` rows for the new session ids and no
   401s for them in the daemon log.
5. Mixed state is expected for a short window; if a critical agent cannot be
   relaunched, roll back (section 1) rather than leave it down.

### Unbounded session credentials (CW-20261016-0218)

A session credential minted while identity was `observe` or `off` can have been
launched with nothing bounding its scopes: with no verified launcher
(`anonymous_external`), or by the daemon itself with no origin session to bound
it (`internal_unattributed`). Credentials minted before the launcher was recorded
and with an empty `created_by` (for example every worker-hosted session before
tether#428) are treated the same way (`legacy_unknown_launcher`). Each holds the
full launch-profile scopes for its 7-day life, so enforcing would only harden
new traffic.

- `tether identity unbounded` (read-only) counts and lists the live ones.
- `tether identity flip ... --to enforce` (and a `rollback` that restores enforce)
  **refuses** while any exist, prints the same counts and list, and changes
  nothing. Relaunch those sessions on a current build first (section 4), or pass
  `--revoke-anonymous` to revoke exactly that list; their sessions then lose
  authentication. A revocation is recorded in the flip's evidence.
- The guard fails closed: if the state database exists but cannot be read, the
  flip is refused. A catalog with no state database yet is not blocked.
- Flips to `observe` or `off` never consult it.
- A launch can land between the guard's scan and the flip. After applying an
  enforce flip the command scans once more and revokes any such straggler, and
  when there are any it appends a `stragglers` record (with the count) to the
  flip log.
- **Daemon-side rule (CW-20261016-0231).** A running daemon applies a flipped
  mode only at restart, but its launch path re-reads the persisted `identity`
  block of `global.yaml` (cached for about 3 seconds; the mode it started with counts too) whenever it decides a
  launch that nothing bounds. Once it is persisted as `enforce` the daemon
  refuses that launch (`ErrLaunchRefused`) immediately, without a restart:
  an `anonymous_external` launch when `identity.mode` or `points.local_api` is
  `enforce`, and an `internal_unattributed` launch when `identity.mode` is
  `enforce` (a single point such as `teams` does not trigger it). If
  `global.yaml` cannot be read at that moment the launch is refused. Launches
  a principal or an origin session bounded never read it and are unaffected.
  This closes the flip-to-restart gap; as belt and braces, still run
  `tether identity unbounded --check` after the restart (it exits non-zero while
  any unbounded credential stands) and re-flip with `--revoke-anonymous` if it
  lists some. The required sequence is: **flip, restart the daemon, run
  `tether identity unbounded --check`, re-flip with `--revoke-anonymous` if it is
  non-zero.**

### Team recovery launches under a global enforce flip (CW-20261016-0233)

A team session is launched inside the verb that formed the team, with a verified
caller, and Tether now records that caller's scopes for the intent
(`team_launch_grants`, first write wins). Recovery and reconcile launches have no
caller; they reuse the recorded grant, so they are bounded by it and attributed
`origin`. A spawned member is also bounded by its parent session's grant.

A root team session whose intent has **no** recorded grant (formed before grants
were recorded) has nothing to bound it. Under `identity.mode: enforce` its
recovery launch is refused as `internal_unattributed` (logged:
`team launch "<key>" refused: no recorded launch grant ...`) and recovery of that
team stops. A single point such as `teams: enforce` does not trigger this, which
is why it works today. **Before a global flip, run this read-only check against
the state database and expect 0:**

```
SELECT COUNT(*) FROM team_port_intents i
 WHERE i.port_kind = 'session' AND i.ended = '' AND i.state IN ('pending','done')
   AND COALESCE(json_extract(i.request, '$.ParentSession'), '') = ''
   AND NOT EXISTS (SELECT 1 FROM team_launch_grants g WHERE g.intent_key = i.intent_key);
```

A non-zero count means that many live root team sessions would not survive a
recovery under global enforce. **A non-zero count blocks the global flip**, the
same as `tether identity unbounded --check` does: re-form those teams (the new
formation records a real grant) or dissolve the dead ones until the count reads
0. Do not backfill a wildcard grant for old teams; that would hand every legacy
team unbounded scope. After a flip, a refused recovery logs its reason, the
intent key and the team run id, which tells the operator what to re-form.

## 5. Declared fail mode per enforcement point

"Fail-open" = the request proceeds on identity-system failure; "fail-closed" =
rejected. Observe/off are fail-open everywhere by construction. Rows below are the
points verified in code; the policy-ADR inventory (27 rows) is not in this repository
and must be mapped onto this table by the owner before the corresponding GO — any
point not listed has **no declared fail mode and cannot be flipped**.

| Point | Switch | Observe/off | Enforce: no/bad credential | Enforce: verifier or audit store down |
|---|---|---|---|---|
| **Tether MCP gateway** (`internal/mcptransport`, opt-in `daemon.mcp_endpoint`) | `points.mcp_gateway` | request path already needs a verified bearer in observe too (401); `off` ⇒ every request 503 `identity_unavailable`, no anonymous service | 401 | request-time verifier failure: 503 in both observe and enforce (**fail-closed per request**). Enforce adds startup **fail-closed**: refuses to start without a verifier or if a verifier probe fails, and is the only mode allowing a non-loopback bind |
| Local API listener (`identity.Middleware`) | `points.local_api` | fail-open | 401 (anonymous not audited) | 503 `identity_unavailable` if verifier unavailable; an audit-persist error → 503 in enforce; `/health` and `/a2a*` exempt |
| Teams (`teamruntime.Principals`) | `points.teams` | **fail-closed always**: not enforce ⇒ `ErrUnauthenticated`, no team authority from attribution | unauthenticated | unauthenticated |
| Session token mint at launch | global `mode` | observe: warn and launch anonymous; off: no token | n/a | **fail-closed** in enforce: launch stops |
| Daemon start / operator bootstrap | global `mode` | observe: degraded, starts | n/a | enforce **refuses to start** on missing/mismatched operator token |
| Native-only resume | global `mode` | follows global | — | enforce path stricter (replacement unavailable) |
| Remote listener | device-only auth (independent of mode) | always enforced | 401 | audit failure → 503 |
| A2A (`/a2a*`) | exempt in phase 1 | own bearer auth | own | own |
| Message `from` stamp (I2), idempotency (I3), reply/delivery authorization (I4) | follow I2–I4 branches | — | — | declare when those land; *not declared here* |
| Scope rejection (route table) | none: `ScopePolicy.Enforce` is not set by the daemon | attribution only | — | — |

Note: for the MCP gateway the observe→enforce flip changes startup readiness and bind rules, not the per-request bearer requirement (already in force in observe); the observe review of its callers is therefore the `mcp_gateway` rows of the endpoint's own tool-call telemetry plus queries 1–2, and the dry run for it is a restart check only.

Flip order (design note section 5 was not readable from this worker; this is the
order the table implies, confirm against the note): `mcp_gateway` first (already
requires verifier/fail-closed), then `teams`, then `local_api`, then global.

## 6. Owner GO record

One record per enforcement point, stored in Tesseract (decision) and referenced in `--evidence`:

```
GO: identity enforce — <point>
Date/time (UTC):            Owner:
Point / switch:             points.<name> observe -> enforce   (or global)
Build / commit:             Daemon restart planned at:
Checklist (section 3):      I1 ✔ I2 ✔ I3 ✔ I4 ✔ read-protection ✔   (task ids + commits)
Observe window:             <from> .. <to>
Query 1 (invalid/unavailable, last 72h):  <n>      audit dropped/failures: <n>/<n>
Query 3 would_allow=0 groups: <n> — fixed: <list>; accepted: <list + initials>
Query 4 uncovered routes:   <n>
Declared fail mode:         <copy row from section 5>
Sessions to relaunch:       <list>
Backup taken:               <path/time>
Flip log head hash before/after: <hash> / <hash>
Rollback:                   tether identity rollback --point <name>; restart. Trigger: <e.g. any 401 for a needed caller>
Decision:                   GO / NO-GO     Signature:
Executed by / at:           <fill after flip>   flips.jsonl line: <copy>
```

A GO covers one point and one window; a rollback needs no GO.

## 7. Dry run (non-production instance)

Rehearse with a throwaway instance; never the live daemon:

```bash
D=/tmp/i6-dry; export HOME=$D/home TMPDIR=/tmp; mkdir -p $HOME
tether init --yes --state-dir $D/state          # temp catalog, DB and own socket
tether --catalog $D/state/catalog daemon run &  # foreground run; socket $D/state/run/tetherd.sock
tether --catalog $D/state/catalog identity flip --point local_api --to enforce --evidence "dry run"
# restart; anonymous GET /sessions -> 401, operator-token GET -> 200
tether --catalog $D/state/catalog identity rollback   # restart (env: unset TETHER_TOKEN/TETHER_MCP_TOKEN); both 200 again
```

Recorded run (2026-10-11, `/tmp/i6-dry`, own socket, `HOME` overridden, `TETHER_TOKEN`/`TETHER_MCP_TOKEN` unset for the verbs): observe
200/200 → flip `local_api` → `/health` `identity.points.local_api=enforce`,
anonymous 401, operator 200 → rollback → anonymous 200, operator 200; flips.jsonl has
both lines. Not covered by the dry run: session relaunch with real provider
agents, `mcp_gateway` and `teams` flips (unit-tested resolution only).
