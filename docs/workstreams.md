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
| `proxy` | proxy has session identity and extraction enabled, through planted flags or saved daemon policy. Can produce observed refs. |
| `none` | proxy has session identity, extraction off. Cannot. |
| `unlaunched` | nothing was planted for this session — the `POST /sessions/bootstrap` path. No proxy carries its id. |
| `unknown` | no reliable stamp was recorded, including rows predating the stamp. **Not** a synonym for `none`. |

`coverage.proxy_attributable` counts how many sessions in the span could
produce one, and `coverage.note` says so in prose when the answer is zero.

Extraction is opt-in. `catalog.defaults.extract_refs` sets the fleet default;
project and launch `mcp.extract_refs` override it, with the launch taking
precedence. The default is false. Hub-local legacy proxy planting emits
`--session` and `--extract-refs` together when enabled; daemon-owned MCP uses
the saved session policy instead. Successful supported tool calls then pass
through `sessionrefs.Service.Record` and persist identifier refs with
`source=proxy`. Failed calls produce no ref, and retries are idempotent.

Enabling the default affects launches that resolve or rebuild their policy;
it does not reconfigure an already-running proxy or reconstruct historical
refs. It adds identifier correlation metadata and per-call writes, without
storing raw arguments or results. `proxy_events` contains schema fingerprints,
not the identifier values needed for backfill. Worker/outpost and externally
started paths require their own session/policy propagation; do not infer
capture coverage from the global setting alone. A `none` or `unknown` stamp
still makes an empty ref list inconclusive.

`source` itself means **observed, not validated** (migration 0024). The proxy
records what it saw; it does not check that the identifier refers to anything.
The HTTP attachment API accepts caller-supplied source values. Treat the source
as provenance — who asserted this — never as authentication or proof that the
caller owns the referenced object.

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
