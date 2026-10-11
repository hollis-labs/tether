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
`identity.mode` (see the fail-mode table). Because session-token minting and
the startup bind rules (`identity.ValidateBind`, MCP listener address checks)
read the **global** mode, flipping a point does not change them.

Commands (all take `--catalog`, default `~/.tether/catalog`):

```bash
tether identity mode                         # configured modes + flip history
tether identity flip --point mcp_gateway --to enforce --evidence "CW-… GO rec <id>"
tether identity flip --global --to enforce --evidence "…"
tether identity rollback                     # ONE command: global + every point -> observe
tether identity rollback --point mcp_gateway --evidence "why"
tether daemon stop && tether daemon start    # apply
tether doctor                                # caller-identity check; curl /health shows identity.mode and identity.points
```

`--evidence` is mandatory on a flip. Each flip and rollback appends one JSON line
to `<state root>/run/identity-flips.jsonl` (0600, append-only):
`{at, action, by, point, from, to, evidence, catalog}`; `by` defaults to the OS
user (`--by` overrides). The audit line is written **before** global.yaml is
replaced (atomic rename, comments preserved), so no change is unrecorded; if the
write then fails, `identity mode` shows the configured value against the log. Back up
`global.yaml` and the state DB before the first flip (operator token and DB must stay a matching pair).

Rollback leaves a restart as the only step; sessions minted under enforce keep
valid tokens in observe, so rollback needs no relaunch.

## 2. Observe-mode review

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
  FROM identity_audit WHERE would_allow = 0 AND at > :since
 GROUP BY 1,2,3,4 ORDER BY n DESC;

-- 4. would_allow IS NULL: verified request on a route/method with no table entry
SELECT method, route, count(*) FROM identity_audit
 WHERE authentication='verified' AND would_allow IS NULL AND at > :since GROUP BY 1,2;

-- 5. verified rows per kind (principal kind from principals)
SELECT p.kind, count(*) FROM identity_audit a
  JOIN (SELECT DISTINCT principal_id, kind FROM principals) p ON p.principal_id=a.principal_id
 WHERE a.at > :since GROUP BY 1;
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
tether --catalog $D/state/catalog identity rollback   # restart; both 200 again
```

Recorded run (2026-10-11, `/tmp/i6-dry`, own socket, `HOME` overridden): observe
200/200 → flip `local_api` → `/health` `identity.points.local_api=enforce`,
anonymous 401, operator 200 → rollback → anonymous 200, operator 200; flips.jsonl has
both lines. Not covered by the dry run: session relaunch with real provider
agents, `mcp_gateway` and `teams` flips (unit-tested resolution only).
