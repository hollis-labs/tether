# Workstreams and the session digest

A **workstream** is the durable container for work that outlives any one
session. A **session ref** records what a session touched. The **digest**
assembles both into the view an operator or a recovery instruction actually
reads.

Sprint SP-20260912-0001 built this in five parts: the container (S1,
`CW-20260912-0059`), refs (S2, `CW-20260912-0060`), proxy-side extraction (S3,
`CW-20260912-0061`), the Tesseract namespace (S4, `CW-20260912-0062`), and this
digest (S5, `CW-20260912-0063`). Design record: `CW-20260912-0023`.

## Why the container exists

A compaction creates a **new session row** carrying `parent_session_id`. So
anything attached to a session id is orphaned by the exact event it was meant
to survive. Attaching to the workstream instead, and inheriting `workstream_id`
structurally down the lineage, is what makes an attachment span
`fresh -> compact -> resume -> fork`.

Inheritance is structural, not remembered: it happens inside `CreateSession`,
which is the only path a session row reaches the database through, so no
caller can skip it. The signal is **`parent_session_id`, not `intent`** —
intent says why a session exists, the parent says what it continues, and
gating inheritance on an enumeration is what lets those two disagree.

## The digest

```
GET  /sessions/{id}/digest
GET  /workstreams/{id}/digest
GET  /workstreams?ref=<kind>:<ref_id>

tether_workstream_digest      session_id= | workstream_id=
tether_workstreams_for_ref    ref=

tether workstreams digest --session <id>
tether workstreams digest --workstream <id>
tether workstreams digest --for-ref torque_task:CW-20260911-0039
```

Both grains return **the same response shape**, so a consumer writes one
parser. They answer different questions:

| grain | question | how often |
|---|---|---|
| session | what did I touch this session, what did I leave behind | the everyday view |
| workstream | what happened across this whole effort, including before a compaction | the recovery view |

The session grain does **not** roll up the lineage — that is what the
workstream grain is, and conflating them would leave no way to ask the
narrower question. A session digest carries its workstream so you can escalate
in one hop.

### Two sections, not one count

Refs are split on `relation`:

- **`left_behind`** — `created` and `updated`. What this work produced. Read
  this first; it is what a reviewer and a recovery instruction both care about.
- **`touched`** — `read` and `referenced`. What it consulted.

"Touched 14 tasks" is noise. "Created 2, updated 1, read 2" is the answer.
Within each section, refs are grouped by `kind`, kinds ordered by most recent
activity, refs within a kind ordered by time. The ordering is deterministic for
the same input: a digest handed to an agent as context should not reshuffle
between calls.

### Filters

| flag / param | effect |
|---|---|
| `--kind` | one ref kind |
| `--relation` | one relation |
| `--source` | one source |
| `--since` | RFC3339 **UTC** lower bound — ask what was in flight, not everything ever |
| `--limit` | maximum refs; the response reports whether it truncated |
| `--left-behind` | print only the created/updated section |

It is `--left-behind`, not `--open-only`. Tether does not hold an open/closed
state for anything it points at, and resolving a Torque task's status would
make Tether a cache of Torque — the boundary migration 0023 draws. The strongest
claim this data supports is "created or updated and not yet resolved by you",
so that is the claim the flag makes.

## Reading an empty digest

**An empty digest is not evidence that nothing happened.** Three independent
reasons a ref can be absent:

1. Nothing happened.
2. The work happened through a path that records nothing — direct MCP children,
   HTTP callers and the CLI all bypass the proxy.
3. **The session could never have produced an observed ref at all.**

The third is the one that misleads, so the digest reports it directly. Every
session in the span carries `ref_attribution`:

| value | meaning |
|---|---|
| `proxy` | session identity and extraction enabled through planted flags or saved daemon policy. Credential provenance is separate. |
| `none` | proxy has session identity, extraction off. Cannot. |
| `unlaunched` | nothing was planted for this session — the `POST /sessions/bootstrap` path. No proxy carries its id. |
| `unknown` | the stamp is absent or the launch path does not support it. **Not** a synonym for `none`. |

`coverage.proxy_attributable` counts how many sessions in the span could
produce one, and `coverage.note` says so in prose when the answer is zero.

Capture is opt-in (`launch.mcp.extract_refs`, then project, then the global
`catalog.defaults.extract_refs`, default false). Existing planted sessions do
not change when the catalog does. Capability is separate from authenticated
provenance: enabling extraction alone does not route traffic through the hub.

Hub-local legacy planting uses `--session` and `--extract-refs`; daemon-owned
MCP uses saved session policy. Supported successful calls pass through
`sessionrefs.Service.Record`; failed calls produce no ref and retries deduplicate.
Capture stores identifiers, not raw arguments or results. Historical
`proxy_events` fingerprints do not contain the identifiers needed for backfill.
The worker relay currently runs SSH to `tether mcp --daemon-only`, which owns
legacy upstream connections and bypasses the hub-owned `/mcp` handler. Its HTTP
ref writes receive `session`, never `proxy`. Calls through the hub handler (including thin `--forward-daemon` clients) can
produce trusted proxy refs. Worker forwarding is a
separate prerequisite for enabling extraction fleet-wide; it is not changed here.
The global setting alone does not establish coverage.

The daemon derives ref `source` from verified identity: `session` for a session
or bound agent credential, `operator` for the actual operator credential, and
unattributed `api` for other legacy HTTP callers. Caller source fields are
ignored; asserting `proxy` always returns 403 `source_asserted` and increments
`tether.refs.source_asserted` (no request data in metric labels).

Migration 0080 downgrades old `proxy` assertions to **`proxy_claimed`**, which is
untrusted and cannot trigger the store's proxy upgrade rule. It preserves row
IDs, refs, timestamps, relations and parents. `agent` is also a legacy assertion.
`proxy` means the hub captured an identifier from successful traffic it forwarded.
The internal capture sink pins the session to the verified credential and uses
the saved extraction policy. Failed calls produce no refs; retries deduplicate.
There is no proxy token or proxy scope, and no HTTP caller can derive this source.
This is provenance, not truth: tool-call arguments remain agent-authored. Calls
that bypass the hub produce no proxy refs. None of these labels validates the
referenced object.
The migration is forward-only; do not run an older binary against the expanded
source vocabulary.

## Truncation

The digest fetches one more ref than the limit so truncation is **detected**
rather than assumed, then reports `coverage.limit` and `coverage.truncated`.
It does not refuse. A silently short list lets a reader conclude work did not
happen because the list ended; an honest partial with a flag does not.

## The reverse lookup

`GET /workstreams?ref=<kind>:<ref_id>` answers "which workstreams touched this
object" — the entry point for *"which Tesseract records came out of the work on
CW-20260911-0039?"*. Resolve, then digest.

**It returns every match and never picks one.** Two separate efforts touching
the same task is ordinary — a fix and a later revert, two agents on one epic —
so a single answer would look authoritative and be wrong whenever the ambiguity
is real.

The selector splits on the **first colon only**, so a ref id containing colons
survives:

```
messaging_urn:msg://agent/agent-mux/agt_x9k2p4qrst
   kind   ->  messaging_urn
   ref_id ->  msg://agent/agent-mux/agt_x9k2p4qrst
```

`ref=` does not combine with `status=` or `workflow_id=`; the request is
rejected rather than silently ANDed, because "which of the workstreams that
touched X are active" requires resolving X first and a caller can filter the
result.

## Worked example: recovery

You are picking up an effort that spanned a compaction and you have the
workstream id.

```
$ tether workstreams digest --workstream 01a09759-f4fa-7a6f-8738-d702b1330fa5
```

```
workstream 01a09759-f4fa-7a6f-8738-d702b1330fa5 (session-correlation)
  status active  workflow SP-20260912-0001

SPAN  3 session(s), crossing a lineage boundary
  sess-a  fresh       completed  refs=2    attribution=none
  sess-b  compact     completed  refs=2    attribution=none  <- sess-a
  sess-c  resume      running    refs=1    attribution=none  <- sess-b

LEFT BEHIND (created / updated)
  torque_task
    created     CW-20260912-0112             agent  2026-09-12T21:18:27Z
    updated     CW-20260912-0063             agent  2026-09-12T21:19:05Z
  git_commit
    created     cc817eb                      api    2026-09-12T20:44:00Z

TOUCHED (read / referenced)
  torque_task
    read        CW-20260912-0061             agent  2026-09-12T16:00:00Z
    read        CW-20260912-0060             agent  2026-09-12T19:00:00Z

TOTALS  5 ref(s)  by source: api=1 agent=4
COVERAGE  limit=500 truncated=false  attribution: none=3
  ! no session here could produce a proxy-observed ref, because ref extraction was not enabled for it (CW-20260912-0112): an empty proxy column says nothing about what these sessions did
```

> **This sample is generated from a fixture, not from production.** It is the
> real output of the renderer — `cmd/tether/workstream_digest_test.go` fails if
> the two diverge — but the data in it is constructed. Measured 2026-09-12:
> the live database holds 129 sessions, every one `intent=fresh`, none with a
> parent, and one workstream containing zero sessions. No production workstream
> spans a compaction yet, so no production example of the interesting case
> exists to show.

Read it in this order:

1. **SPAN** — three sessions, and `crossing a lineage boundary` says the
   roll-up actually spanned a compaction rather than merely being large. At one
   session it would say `no lineage boundary crossed`, which is a first-class
   answer and not a degenerate one: a container that has not compacted yet is
   simply one that has not compacted yet.
2. **LEFT BEHIND** — what to resolve. Hand these ids to Torque and Tesseract;
   Tether records the correlation and never resolves it.
3. **COVERAGE** — before concluding anything from what is missing.

To start from an identifier instead of a workstream:

```
$ tether workstreams digest --for-ref torque_task:CW-20260911-0039
```

## What the digest does not do

- **It does not resolve what it points at.** A `torque_task` ref is an id, not
  a status. Tether records correlations for systems it does not own (migration
  0023), so "is it still open" is a question for Torque.
- **It does not see git.** Commits and PRs happen through Bash, which never
  touches Tether. Those refs arrive through the attach API from agent-setup's
  commit / PR / end-session hooks.
- **It cannot report refs orphaned by a deleted session.** The workstream
  roll-up joins through `sessions.workstream_id`, and `ON DELETE CASCADE` is
  declarative only under ADR 0008, so a ref whose session row is gone leaves
  the roll-up silently. Latent — there is no production session-delete path —
  and retention is tracked at `CW-20260912-0069`.

## Where the content lives

Tether stores **no** note bodies, scratch or todo text. It resolves where those
belong in Tesseract workspace and records the returned item or revision id as a ref:

```
GET /sessions/{id}/workstream-namespace?project=<proj>&owner=<owner>&tail=scratch
  -> {"namespace": "project/<proj>/workspace/scratch", "workstream_id": "<workstream-id>"}
```

Workstream ID is an **attribute**, never a namespace partition (Chrispian's
ruling, CW-20260912-0062). Contained material belongs in Tesseract's declared
workspace domain:
- Project-owned scratch: `project/<declared-project-id>/workspace/scratch`
- Tether's own cross-project scratch: `app/tether/workspace/scratch`

Content is written directly to Tesseract with `workstream_id` attached as an
attribute, and the returned identity attached to Tether via `kind=tesseract_item`
or `kind=tesseract_revision`.

## Listing workstreams

`GET /workstreams` returns bounded live pages, default 100 and maximum 1000.
Follow `next_cursor` with the same status/workflow/ref filters until `complete`
is true; `has_more` distinguishes a partial page. Equal creation timestamps use
ID as a deterministic tie-breaker. This is live exhaustion, not snapshot
completeness: an insertion ahead of the cursor needs a fresh scan. The same
page envelope and continuation are available in the CLI and MCP; the legacy
client slice helpers traverse every page. See the API reference for invalid
cursor rules and ref-filter exclusivity (CW-20261018-0008).
