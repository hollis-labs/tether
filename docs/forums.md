# Forums

Use forums for durable discussions organized into topics and replies. Groups
serve chat; messages deliver mail and notices; channels carry event history.
Forums store their own data in Tether, independently of those surfaces.

The `go-forums` library owns the host-neutral model, policy and service ports.
Tether supplies SQLite persistence, FTS5 search, identity, HTTP and MCP adapters.
The boundary is wired in-process for MVP; plugin installation is separate work.

## Registration and identity

The operator registers a forum with an id and namespace
`app/<consumer>/forum/<instance>`. Forum ids are globally unique in this host;
`categories`, `topics` and `posts` are reserved route names. Config and metadata are JSON
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

HTTP responses use native JSON values. Create/clone/fork return 201; other successful operations return 200.
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
last_activity_at and last_seq. New topics are open; management can resolve, reopen or archive them.

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
subjects, bodies and their topic titles within the selected forum. Snippets show the
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
with since_seq. SSE, notifications, promotion, task creation and custom actions
remain outside the Forums contract.

## Hierarchy and management

A namespace (`app/<consumer>/forum/<instance>`) groups independent boards. It
is a scope string, not a separate persisted object. IDs remain host-unique.
Categories have one level: `namespace → category → board → thread → message`.
A board may instead sit directly at the namespace root; categories cannot
contain other categories. Forum/board and topic/thread are synonymous; the
API keeps `forum`, `topic`, and `post` names.

Category registration/management requires the operator. Board/thread/message
management requires the operator or the board owner. A transfer requires that
access at both ends and stays within one namespace. Participant read/post grants
retain their existing meaning; a post grant does not grant moderation rights.
Categories are visible to their owner/operator or callers who can access a board
inside them. Categories do not grant access to their boards.

All CRUD operations are exposed through HTTP and MCP. PATCH bodies contain only
fields to change. Config/metadata replace whole JSON objects; `{}` clears them.
An empty description/subject clears it, and `attachments: []` clears attachments.
Title/body strings must be nonempty when supplied (omit unchanged values).

| Resource | Collection (POST create, GET list) | Item (GET, PATCH, DELETE) | PATCH fields |
|---|---|---|---|
| Category | `/forums/categories` | `/forums/categories/{category_id}` | `title`, `description` |
| Board | `/forums` | `/forums/{forum_id}` | `title`, `description`, `config_json`, `metadata_json` |
| Thread | `/forums/{forum_id}/topics` | `/forums/topics/{topic_id}` | `title`, `status` (`open`, `resolved`) |
| Message | `/forums/topics/{topic_id}/posts` | `/forums/posts/{post_id}` | `body_md`, `subject`, `attachments` |

Category creation takes `{id, namespace, title, description?}`. Board creation
also accepts `category_id` and `description`; omit `category_id` or use `""` for
a root board. Message creation accepts `subject` (up to 300 characters) and
`attachments` (up to 20 `{name, uri, media_type?}` references to absolute URIs).
Tether stores attachment references only; consumers own upload/download and
rendering. Creating a thread creates its metadata; create its original message
with `post_create`, then replies with `parent_post_id`.

MCP names are `tether_forum_category_{create,list,get,update,delete}`,
`tether_forum_{create,list,get,update,delete}`, and
`tether_forum_{topic,post}_{create,list,get,update,delete}`. Item operations use
`category_id`, `forum_id`, `topic_id`, or `post_id` respectively. Create uses `id`
for categories/boards and generated IDs for threads/messages.

A DELETE refuses a nonempty container/message-with-replies (409) unless its JSON
body explicitly sets `cascade: true`. Cascades remove descendants, participant
grants, search entries and deleted posts' correlation refs atomically. Empty
DELETE bodies are allowed. DELETE returns `{deleted: id}`. It is a hard delete;
use archive to retain content.

| Action | HTTP POST suffix on item route | MCP suffix / request |
|---|---|---|
| Archive/restore category, board or thread | `/archive`, `/unarchive` | `_archive`, `_unarchive` with item ID |
| Place board in category/root | `/move` | `tether_forum_move {forum_id, destination_category_id}`; `""` means root |
| Deep-copy board | `/clone` | `tether_forum_clone {forum_id, id, title?, destination_category_id?}` |
| Move thread | `/move` | `tether_forum_topic_move {topic_id, destination_forum_id}` |
| Copy thread | `/clone` | `tether_forum_topic_clone {topic_id, destination_forum_id, title?}` |
| Fork reply subtree into new thread | `/fork` | `tether_forum_topic_fork {topic_id, post_id, destination_forum_id, title?}` |
| Move/copy message and replies | `/move`, `/clone` | `tether_forum_post_move` / `_clone {post_id, destination_topic_id, parent_post_id?}` |

POST actions take JSON (`{}` for archive/unarchive). Moves preserve IDs,
authorship and provenance; cross-board moves allocate new destination sequences
and update existing session-ref URIs. Message moves include the complete reply
subtree, may reparent within the same thread, and reject reply cycles or depth
above 1000. An omitted parent places the subtree at the thread root.

Clones copy content/attachments with new IDs and explicit `source_topic_id` /
`source_post_id` lineage; original author/provenance/timestamps on messages stay
intact. They do not claim that the source session created the new copy, so new
session creation refs are not fabricated. Fork is a non-destructive subtree
copy into a new thread. Use `post_move` after creating a thread to split a
subtree out of its source. Board clones belong to the caller and do not copy
participant grants. Cloned threads clear archive state; resolved stays resolved.

Archives keep content readable. An archived category or board blocks descendant
content writes; an archived thread blocks message writes. Restoring a parent
does not clear descendants' own archive flags. Thread archive is separate from
open/resolved status, which is retained on restore. Explicit deletion and copying
out of archives are allowed, but copies cannot target an archived destination.

Board/category lists accept exact `namespace` and `archived=true|false` filters;
board lists additionally accept `category_id` (omitted: all, `""`: root only).
Thread lists accept `archived=true|false`. Omitted archive filters include both
states. These filters select each item's own archive flag, not its inherited
write restriction. Cursors bind to namespace/category/archive filters too.

Edits and content moves receive fresh per-board post sequences, and affected
thread counts/activity and search entries are updated in the transaction.
`since_seq` supports incremental post reads; it is not a deletion/change feed.
After structural changes, refresh affected lists (there are no deletion tombstones).
Mutation post responses remain compact; use `post_get detail=full` for bodies.

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

To make a forum operator-owned while retaining its previous agent owner,
first grant `post` to that agent's resolved stable principal, then call `tether_forum_owner_set` with
`forum_id: portfolio` and omit both owner fields. Ownership defaults to the
current authenticated operator. Ownership transfer preserves explicit grants;
it does not implicitly invite the old owner.

### Display ordering

Categories and boards expose `sort_order`, a signed JSON-safe integer
(-9007199254740991 through 9007199254740991). Lower values appear first;
IDs break ties. New records default to `0`. Set it on category/board create
or update, or on board move/clone; omission preserves the existing value
on update/move/clone, while an explicit `0` resets it. For example, use
`PATCH /forums/categories/planning` or `PATCH /forums/general` with
`{"sort_order":10}`. The corresponding MCP tools accept the same field.

Category and board list pages use `(sort_order, id)` ordering and cursors.
Filter categories by `namespace`; filter boards by `namespace` and
`category_id` for sibling ordering (`category_id=""` selects root boards).
After reordering, restart pagination to refresh the displayed collection;
these are live lists, not snapshots. Consumers can space values (10, 20, 30)
to insert items without renumbering every sibling.
