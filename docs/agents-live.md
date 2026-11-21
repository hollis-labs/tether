# Live agents (`tether_agents_live`)

One read for the Ops Agents/Fleet views: every live agent, whether Tether
launched it or a kit booted it. MCP `tether_agents_live` and HTTP
`GET /agents/live` share one implementation and the same validation. Read scope
only; unauthorized callers are refused by the existing remote route policy.

## Query

| Param | Default | Meaning |
|---|---|---|
| `host` | any | Exact machine name, case-insensitive. Outpost sessions use the worker label. |
| `kind` | any | `tether_session` or `kit_seat`. Anything else is `400 invalid_request`. |
| `include_stale` | `false` | Include rows whose presence is older than the threshold. |
| `stale_after_seconds` | `600` | Freshness threshold, `1..86400`; out of range is `400`, never clamped. |
| `limit` | `200` | `1..1000`. |
| `cursor` | first page | Opaque `next_cursor` from a previous response; invalid is `400`. |

Filters compose. An empty result is `{"agents": [], "count": 0, ...}`.

## Response

`{agents, count, limit, truncated, next_cursor, observed_at, stale_after_seconds}` (the MCP tool adds `ok`).
`truncated` is true when more rows matched than `limit`, and `next_cursor` then continues the listing (paging is offset-based over a freshly built view, so rows can shift between pages). Rows are ordered by newest
presence, rows with no presence last.

| Field | Notes |
|---|---|
| `id` | Stable row key: `session:<id>`, `seat:<urn>:<session>` or `seat:<urn>` (presence only). |
| `kind` | `tether_session` or `kit_seat`. |
| `agent` | `{urn, name, ref}`. `urn` is the catalog `urn` (else `msg://agent/agent-mux/<id>`) for sessions; `name` is the logical agent id for sessions, the registered `display_name` (else the URN's last segment) for seats; `ref` is the seat's declared work reference (e.g. `task:<id>`), omitted when none. |
| `host` | any | Exact machine name, case-insensitive. Outpost sessions use the worker label. |
| `kind` | any | `tether_session` or `kit_seat`. Anything else is `400 invalid_request`. |
| `include_stale` | `false` | Include rows whose presence is older than the threshold. |
| `stale_after_seconds` | `600` | Freshness threshold, `1..86400`; out of range is `400`, never clamped. |
| `limit` | `200` | `1..1000`. |
| `cursor` | first page | Opaque `next_cursor` from a previous response; invalid is `400`. |

Filters compose. An empty result is `{"agents": [], "count": 0, ...}`.

## Response

`{agents, count, limit, truncated, next_cursor, observed_at, stale_after_seconds}` (the MCP tool adds `ok`).
`truncated` is true when more rows matched than `limit`, and `next_cursor` then continues the listing (paging is offset-based over a freshly built view, so rows can shift between pages). Rows are ordered by newest
presence, rows with no presence last.

| Field | Notes |
|---|---|
| `id` | Stable row key: `session:<id>`, `seat:<urn>:<session>` or `seat:<urn>` (presence only). |
| `kind` | `tether_session` or `kit_seat`. |
| `agent_urn`, `name` | Catalog `urn` (else `msg://agent/agent-mux/<id>`) and logical agent id for sessions; registered `display_name` (else the URN's last segment) for seats. |
| `host` | Omitted when unknown. Hub sessions use the daemon's hostname; outpost sessions the worker label; kit seats the registered `host`. |
| `state` | `awake` (a busy/idle observation, or a kit seat heartbeat within 10m), `sleeping` (an explicit `paused`/`offline` declaration, or a kit seat heartbeat 10m-2h old), `stale` (kit seats only: heartbeat older than 2h) or `unknown`. Tether-session state is never inferred from lifecycle timestamps. |
| `raw_state` | `busy` or `idle` when known; otherwise the session state (`running`/`detached`) or the seat's declared status. |
| `last_activity_at` | `null` when none was observed. `last_activity_source` says where it came from. |
| `last_activity` | One-line summary, only when known (the seat's declared status reason, or a registered `summary`). |
| `session_id`, `run_id`, `task_id`, `workstream_id` | Omitted when not known/stamped. |
| `presence_at`, `presence_source` | Newest presence evidence, or `null`. |
| `stale` | See below. |

### State and activity provenance

- Tether session: the runtime's live state (processing → `awake`/`busy`, idle → `awake`/`idle`);
  `unknown` when the runtime is not registered or is stopping. `last_activity_at` is the session's observed
  activity (`session_runtime`), as in `tether_session_health`.
- Kit seat with a declared status (`tether_status_set`): `working` → `awake`/`busy`,
  `available` → `awake`/`idle`, `paused`/`offline` → `sleeping`; every other declared state → `unknown`
  with `raw_state` kept. `last_activity_at` is that declaration's update time (`seat_status`).
- Kit seat with no declaration (or an expired one): `state` comes from the heartbeat, the newest of
  its registration, watcher and binding evidence: `awake` within `agents_live.kit_seat_awake_after`
  (default 10m), `sleeping` up to `agents_live.kit_seat_stale_after` (default 2h), `stale` beyond.
  `last_activity_at` is the last registration (`registration`). Name is the registered `display_name`.

## Freshness

`stale` is true when the newest presence evidence is more than the threshold old
(exactly at the threshold is fresh) or when there is no evidence. Evidence:

- `session_runtime`: the runtime confirms the session is alive now.
- `session_activity`: the session's last observed activity.
- `watcher`: an open `GET /messages/watch` stream for the agent's mailbox (evidence "now"
  while connected, so a quiet watcher is not stale; after disconnect it decays from the
  disconnect time).
- `binding`: a non-revoked, unexpired runtime binding for the agent URN, dated by its last
  renewal. Revoked and expired bindings are ignored.
- `registration`: the kit seat's last registration. It is evidence only at the time it was
  made, so an old registration alone goes stale.

For kit seats the threshold is `max(stale_after_seconds, kit_seat_stale_after)` (default 2h), so a
sleeping seat stays listed until it is `stale`; an explicit longer `stale_after_seconds` still applies.

An agent URN seen only through a watcher or binding appears as a `kit_seat` row with no
session. A URN that already has session or seat rows contributes presence to them and
adds no extra row. Terminal sessions are never listed.

## Kit seats: registration

`POST /correlation/register` (local operator) now also records the seat in the local
`kit_seats` table (migration 0117), keyed by `(agent_urn, session)`, whether or not
outbound Registry registration is configured. Optional `host`, `display_name` and
`summary` are local-only descriptive facts, never sent to the Registry. Re-registering
refreshes `last_seen_at`; empty facts never erase known ones. Rows unseen for 7 days are
pruned. Seats should re-register periodically (for example from a hook) to stay fresh.

### Configuration

In `global.yaml` (Go duration strings; empty or invalid use the defaults):

```yaml
agents_live:
  kit_seat_awake_after: 10m   # heartbeat within this -> awake
  kit_seat_stale_after: 2h    # heartbeat older than this -> stale; between -> sleeping
```

Seats heartbeat by re-running `tether correlation register --urn ... --session ...` (optionally
with `--host`, `--display-name`, `--summary`, `--activity-summary`).

## Limitations

- Watcher presence is held in daemon memory: a daemon restart forgets it until streams
  reconnect. The in-process MCP fallback (no daemon) sees no watchers.
- Registration is best effort and not durable at the source; a seat that never registers
  and never watches is invisible here.
- Presence is per agent URN, not per session: two seats sharing a URN share watcher evidence.
