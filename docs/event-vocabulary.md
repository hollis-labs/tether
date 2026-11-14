# Event vocabulary: summary, cell and the minimal kinds

Tether's `events` table is the shared bus (design v2 section 11). This page is the
contract other seats build to, for example Tachyon's activity cards
(CW-20261114-0019).

## Envelope fields

| Field | Wire | What |
| --- | --- | --- |
| summary | top-level `summary` on `/environment/events`, `tether_events_wait`; `summary` on `GET /events` and `tether_events_history` | One line saying what happened: at most 200 characters, no newlines, run through the redactor. Built from the kind and low-risk payload fields (names, ids, states, counts), never from message bodies, prompts or tool input. A producer may set its own; Tether cleans it the same way. |
| cell | `scope.cell` on `/environment/events`; `cell` on `GET /events` | The team the acting seat belongs to, from the catalog agent's `cell:`. Empty when the event has no catalog agent or the agent has no cell. Filter with `scope.cell=<cell>` or the `cell` argument of `tether_events_wait`. |

Where the cell comes from, first match wins: the event's logical agent; else the
catalog agent whose `urn:` (or `msg://agent/agent-mux/<id>`) is the event's
agent URN; else, for `message.*`, the sender of `sent`/`queued`/`redriven`/`canceled`
and the recipient of everything else. A catalog agent inherits `cell:` through
`extends`.

```yaml
# catalog agents/<id>.yaml
id: photon-tether
cell: photon        # lowercase slug: letters, digits, '-' or '_', starting with a letter
```

Rows written before this change have no summary or cell. Producers posting to
`POST /events` can send them once libs ships `runtimeevents.Event.Summary` and
`EventScope.Cell` (hollis-labs/libs#128) and Tether maps them at ingest.

## Minimal kinds

| Kind | Producer | When | Payload |
| --- | --- | --- | --- |
| `session.state_changed` | Tether | session lifecycle | `{from, to, ...}` |
| `session.cleared` | Tether | the hub submitted `/clear` to the session | `{source:"clear", via:"turn_input"}` |
| `session.compacted` | Tether | a worker-hosted Claude session's stream marked a compaction (`compact_boundary`) | `{source:"compact", trigger?:"auto"\|"manual", pre_tokens?}` |
| `seat.waiting` | Tether | the session's first open approval or question | `{waiting_for:"approval"\|"question", request_id}` |
| `seat.unblocked` | Tether | its last open request closed | `{waiting_for?, request_id?, resolution:"resolved"\|"turn_ended"\|"session_ended"}` |
| `message.sent`, `message.read`, `message.consumed`, ... | Tether | mail lifecycle (already emitted; now with a summary such as `@photon-tether read request from @archie`) | `message.lifecycle@1` |
| `task.state_changed`, `run.state_changed`, `checkpoint.state_changed` | Torque | task lifecycle | Torque's |
| `tool.call.started` / `tool.call.finished` | Tether | MCP tool calls | `{tool, server, ok, duration_ms, ...}` |

Not yet emitted, owned elsewhere: `deploy.*` (Cerberus), `pr.*` (Tachyon forge
index), `handoff.written` / `decision.captured` (Tesseract), `task.claimed`
(Torque).

Known gaps: `session.compacted` is seen only on worker-hosted sessions (the hub
parses their output); `session.cleared` only when the hub submitted the `/clear`.
`seat.waiting` follows `session.request_status`, which today comes from Codex
app-server requests; a session open across a daemon restart closes without a
`seat.unblocked`.

## Retention

Raw provider output (`session.output`, about 98% of rows) is admitted to the
`R-debug` class at insert, and the hourly sweep deletes R-debug rows after
`daemon.events_retention.debug_days` (default 7). Every other kind is
unclassified and stays on `daemon.events_retention.days` (default 90). Rows
written before the change are unclassified and age out on the 90-day window;
nothing is backfilled. Non-debug volume is `retention_class IS NOT 'R-debug'`.
