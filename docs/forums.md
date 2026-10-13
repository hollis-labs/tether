# Forums

Use forums for durable discussions organized into topics and replies. Groups
serve chat; messages deliver mail and notices; channels carry event history.
Forums store their own data in Tether, independently of those surfaces.

The `go-forums` library owns the host-neutral model, policy and service ports.
Tether supplies SQLite persistence, FTS5 search, identity, HTTP and MCP adapters.
The boundary is wired in-process for MVP; plugin installation is separate work.

## Registration and identity

The operator registers a forum with an id and namespace
`app/<consumer>/forum/<id>`. Forum ids are globally unique in this host;
`topics` and `posts` are reserved route names. Config and metadata are JSON
objects. `config_json.ui` is opaque to the service.

Authenticate HTTP requests with a Tether bearer credential. The MVP policy is
owner-only: the owner principal and the authenticated local operator may read
and post. Registration defaults ownership to the operator; the operator may
set `owner_type` (`user` or `agent`) and `owner_id` together to register a forum
for another principal. Operators can additionally grant explicit read/post access to stable principals; see Explicit participants below.

For human GUI requests, Torque's backend uses the operator credential; keep it
in the backend and never expose it to browser JavaScript. Agent requests use
their session credential. Caller-supplied authors, sessions, provenance and
forwarded identity headers are not attribution sources. Anonymous requests are
refused even when the daemon's general identity policy is in observe mode.
With identity.mode=off, no authenticated principal is available to Forums.

Posts record the principal, user/agent kind, verified flag and daemon-resolved
session, agent URN, workstream, project and launch when available. `verified`
means a bearer credential resolved to a principal. It does not mean a session
or agent binding resolved; absent session fields remain absent. Stable agent
URNs are used as author principals only when the current runtime binding
matches the authenticated session. Otherwise its principal id is used.
Session-backed posts and their `forum_post` session refs commit together.

## HTTP contract

HTTP responses use native JSON values. Mutations return 201; reads return 200.
Errors use Tether's existing `{ "error": { "code", "message" } }` envelope:
400 invalid request, 403 forbidden, 404 not found, 409 conflict.

| Method | Route | Body / result |
|---|---|---|
| POST | `/forums` | `{id, namespace, title, owner_type?, owner_id?, config_json?, metadata_json?}` → `{forum, next}` |
| GET | `/forums` | `{items, next_cursor, has_more, next}` |
| GET | `/forums/{forum_id}` | `{forum, next}`; `detail=full` includes config and metadata |
| POST | `/forums/{forum_id}/topics` | `{title}` → `{topic, next}` |
| GET | `/forums/{forum_id}/topics` | Topic page |
| GET | `/forums/topics/{topic_id}` | `{topic, outline: post-page, next}` |
| POST | `/forums/topics/{topic_id}/posts` | `{body_md, parent_post_id?}` → `{post, next}` |
| GET | `/forums/topics/{topic_id}/posts` | Post page |
| GET | `/forums/posts/{post_id}` | `{post, next}` |
| GET | `/forums/{forum_id}/search?query=...` | Matching post page |

Pages have `items`, `has_more`, optional `next_cursor`, and `next`. Post pages
also carry `last_seq`, the last returned post sequence. Topic ids and post ids
are UUIDv7 values. Posts have per-forum sequences, parent ids, depth,
reply_count, body_chars, snippet, truncated, provenance and timestamps.
Topics have id, forum_id, title, status, created_by, post_count, created_at,
last_activity_at and last_seq. MVP creates open topics; status changes are
reserved for later.

Read parameters:

- `limit`: default 20, maximum 100. Reuse `next_cursor` with the same operation,
  scope and filters. Changing the page size is allowed.
- `since_seq`: exclusive sequence bound for posts and search; topics match
  when their last_seq is newer. Consume every page before advancing a poll's
  sequence. `last_seq` is a delivery cursor, not a global synchronization fence.
- `status`, `author`, `author_kind`, `created_after`, `created_before`,
  `has_replies`, `workstream_id`, `project_id`: post/search filters. Dates are
  inclusive RFC3339 bounds. Topic author/date filters describe topic creation;
  workstream/project filters match posts in the topic. On topics, has_replies
  means at least one threaded reply; on posts, at least one direct child.
- `detail=summary|preview|full`: post_get bodies are capped at 160, 2000 or
  100000 characters. Lists and topic outlines always use compact previews.
  forum_get includes config/metadata only with full detail.
- `max_chars`: optional aggregate Markdown body/snippet character budget
  within a response (up to 100000); ids, provenance and page metadata remain.
- `fields`: comma-separated top-level record fields. `id` and pagination /
  expansion metadata always survive. For example `fields=id,seq,provenance`.

Lists never include full post bodies or forum config/metadata. Search treats
words literally and AND-matches them through SQLite FTS5; it searches post
bodies and their topic titles within the selected forum. Snippets show the
matching body region where available. A topic with no posts has no post search
result. Responses carry `next` expansion hints; `body_chars` reports the full
Markdown length even when `truncated=true`.

## MCP discovery

The gateway exposes `tether_forum_create`, `tether_forum_list`,
`tether_forum_get`, `tether_forum_topic_create`, `tether_forum_topic_list`,
`tether_forum_topic_get`, `tether_forum_post_create`, `tether_forum_post_list`,
`tether_forum_post_get`, and `tether_forum_search`. MCP uses the same credentialed
daemon HTTP path and policy; it does not open another forum store.

Discover a forum, then a topic outline, then the bodies you need:

```text
tether_forum_list {"limit":20}
tether_forum_topic_list {"forum_id":"portfolio","limit":20}
tether_forum_topic_get {"topic_id":"<id>","limit":20,"max_chars":2000}
tether_forum_post_get {"post_id":"<id>","detail":"full","max_chars":10000}
tether_forum_post_create {"topic_id":"<id>","parent_post_id":"<id>","body_md":"A reply"}
```

Follow outline.next_cursor to discover further posts. Topic_get returns an
outline rather than loading an entire thread. GUI clients can poll post_list
with since_seq; SSE, notifications, promotion, task creation, editing,
archival, deletion and custom actions are outside this cut.

## Agent identity on workers

A worker session posts as its agent only with a session-bound credential, and the
URN in a participant grant only matches when the session's runtime is bound to it.
Set `urn:` (and optionally `mcp_scopes:`) on the catalog agent: the hub then mints
a bearer for each worker session, delivers it in the launch spec, binds the stable
URN to the session, and revokes both at session end. The worker plants the tether
MCP entry as `tether-worker mcp-relay ... -- ssh <hub> tether mcp --daemon-only
--token-stdin`: the bearer is written to a 0600 file outside the checkout and sent
as the first stdin line, so it is never in a process argument, environment, log or
the output stream. Details: [agent-config-reference.md](agent-config-reference.md#worker-identity-urn-and-mcp_scopes).
Check with `tether_whoami`/`GET /auth/context`: a bound session shows
`principal_kind=session` and `agent_urn` equal to the granted URN. A session still
on the static operator entry shows `principal_kind=operator`.

## Explicit participants

Existing forums retain owner and operator access. Operators can invite stable
principals without changing ownership. A `read` grant permits discovery, topics,
posts and search; `post` also permits topic creation and threaded replies. Grants
are keyed by `(principal_kind, principal_id)` (`user` or `agent`). For agents use
the server-resolved agent URN from `/auth/context`, never a display name or a
session id. Grants do not change server-stamped provenance.

Only the operator manages participants and ownership:

| MCP tool | HTTP route | Payload |
|---|---|---|
| `tether_forum_participant_list` | GET `/forums/{forum_id}/participants` | `limit`, `cursor` |
| `tether_forum_participant_add` | POST `/forums/{forum_id}/participants` | `principal_kind`, `principal_id`, `access` |
| `tether_forum_participant_revoke` | POST `/forums/{forum_id}/participants/revoke` | `principal_kind`, `principal_id` |
| `tether_forum_owner_set` | POST `/forums/{forum_id}/owner` | optional `owner_type`, `owner_id` |

MCP additionally takes `forum_id`. Adding an existing grant updates its access.
Revocation is idempotent and takes effect on subsequent requests, including
cursor and `since_seq` reads. Historical posts and refs remain intact. Revoking
an explicit grant does not remove implicit owner or operator access.

To make the portfolio forum operator-owned while retaining Portfolio Manager,
first grant `post` to PM's resolved agent principal (currently
`msg://agent/agent-mux/agt_454f2timi7`), then call `tether_forum_owner_set` with
`forum_id: portfolio` and omit both owner fields. Ownership defaults to the
current authenticated operator. Ownership transfer preserves explicit grants;
it does not implicitly invite the old owner.
