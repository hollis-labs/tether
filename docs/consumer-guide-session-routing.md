# Session routing: a consumer guide

Tether captures a managed session's user-facing turn output and, when its launch
opts in, publishes selected kinds into a named channel. Consumers choose what to
display or act on. Tether never registers consumers, chooses their relevance
rules, or decides whether Chrispian needs to see a message. Tangent, Nanite, sysop
and an interface agent can independently consume the same channel.

Routing is off by default. Routes target channels. Replies always go back to the
sender and are delivered as the next turn; interrupt is optional. Consumers do
not inject turns themselves after submitting a reply.

## Availability and deployment

This guide describes interfaces merged through Tether main `508c922` and
go-tether-client v0.10.0 (`6477021`, includes PR #9/#10). A merge does not
establish that your daemon is deployed. Check deployed capabilities.

| Surface | Merged implementation |
|---|---|
| Route configuration | CW-20261002-0063, PR #131 |
| Named channel history and SSE | CW-20261002-0069, PR #133 |
| Capability endpoint and MCP consumer tools | CW-20261002-0066, PR #137 |
| Go channel/capability/reply client | go-tether-client PR #9/#10 |
| `session.turn_output` publisher | CW-20261002-0062, PR #136; hardening and follow-ups #143/#145/#146 |
| Turn-output attachment to channels | CW-20261002-0064, PR #141 |
| Reply-to-sender delivery | CW-20261002-0065, PR #142 |
| Interrupt before next-turn delivery | CW-20261002-0067, PR #140 |
| Question/approval detection | CW-20261002-0073; tagged libraries and runtime-specific daemon feeds |

The daemon installs the publisher, channel router, reply dispatcher and interrupt
path. Runtime and mode support still varies; query capabilities for the sender
session. Channels also accept ordinary publications independently of session routing.

The [API reference](api/README.md) specifies the HTTP interfaces. The
[launch guide](caller-launched-sessions.md) explains catalog and override inputs.

## Enable routing on a launch

A route contains a channel **name** and selected output kinds:

```json
{"route":{"channel":"ops","kinds":["question","approval"]}}
```

Put `route` on a catalog agent or launch, in an agent YAML file or inline agent,
in the inline JSON `override` string, or directly on the session-create request.
Each explicit route replaces the lower-precedence route rather than merging kinds.
Precedence, lowest to highest: catalog agent, catalog launch, agent file, inline
agent, override JSON, explicit API/CLI route. For example:

```bash
tether launch --launch demo-launch --override '{"route":{"channel":"ops","kinds":["final"]}}'
```

`--override` is a JSON string, not a file path. The resolved route is persisted
with the **session at create**, in `sessions.route_json`; later catalog edits do
not change it.

No route means no routing. With a route, omitted or null `kinds` means all four supported
kinds: `final`, `question`, `approval`, `failure`. An explicit empty list selects
none. `terminal` can appear on the event bus but is **not routable**.

Use an existing launch id with the CLI:

```bash
tether launch --launch demo-launch --route ops --route-kinds question,approval
```

Omit `--route-kinds` for the four defaults; `--route-kinds ""` selects none.
`--route-kinds` requires `--route`. Names and kind values are validated.

Over HTTP, create a session with `POST /sessions` and then launch it with
`POST /sessions/{id}/launch`. The create body uses `launch`, not `launch_id`:

```json
{"launch":"demo-launch","route":{"channel":"ops","kinds":["final","failure"]}}
```

A configured route records intent. It does not prove that publication, the
router, or a runtime's question/approval feed is installed.

## Channels, cursors and retention

A stable channel handle is a case-sensitive name matching
`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`: 1–64 ASCII characters. The derived address is
`msg://service/local/channel/<name>`. Responses include both `name` and `address`;
URL paths and route configuration are keyed by name.

A successful publication creates the channel implicitly. An unknown valid name
has empty history and can be subscribed to before its first publication. Only
envelopes addressed to the canonical channel address enter public history.
A private mailbox message with the same `channel` label stays private.

No membership is needed to read or subscribe. Caller identity still follows the
daemon's authentication mode: a verified principal takes precedence over an
asserted `as` URN. Without a verified bearer, `as` is required on channel list,
history and subscribe: missing/invalid values return 400 `invalid_request`.
Under ENFORCE a verified bearer is required instead; `as` cannot replace it.
The default channel authorization hooks operate in observe
mode; this does not establish future grants or enforcement policy.

| Read | Behavior |
|---|---|
| `GET /channels?as=<caller>` | Published names and derived addresses, sorted by name |
| `GET /channels/{name}/messages?as=<caller>&since=0&limit=100` | Messages strictly after `since`, oldest first; returns `next_since` |
| `GET /channels/{name}/messages?as=<caller>&last=20` | Latest 20, returned oldest first; excludes `since` and `limit` |
| `GET /channels/{name}/subscribe?as=<caller>&since=0` | SSE replay, then live publications |

An omitted history `limit` defaults to 100. Explicit `limit` and `last` must be
1–1000; values outside that range, including zero, return 400 `invalid_request`
rather than being capped. MCP read/list bounds are also validated.
Publication `seq` determines order, even when timestamps disagree. Sequences may
have gaps; do not calculate the next message id from a count. Continue history
with `since=next_since`. Channel cursors and event-bus cursors are separate.

For SSE, omit `since` for live-only; `since=0` replays all retained structural
history. Reconnect uses the **greater** of query `since` and `Last-Event-ID`.
A live-only subscription starts with an `id: <high-water>` frame with no message
body. A cursor ahead of the high-water mark is clamped, so EventSource can keep
reconnecting. Negative or malformed cursors are invalid. SSE messages carry
`event: message`, `id: <seq>` and a JSON history item in `data`.

Reading never consumes, acknowledges, archives or marks a channel publication
read. Mailbox operations addressed to a channel URN or channel publication,
including inbox, consume, cancel, archive, read, claim/ack/nack, redrive and
notify, reject with `channel_not_mailbox` and leave history unchanged.

Channels follow the explicit audited messaging purge policy. A channel
publication has no mailbox delivery obligations, so it can be eligible for an
explicit purge immediately. Purge removes body and metadata but preserves
structural history and sequence cursors. History and SSE replay include
`purged: true` and `purged_at`; an empty published body is not a purge tombstone.
There is no automatic deletion on read and no automatic 30-day channel expiry.

Selected turn output is stored once as a staged message and attached to the channel using the same message id. Staging is hidden
from mailbox and sysop inboxes. A never-attached staged body becomes eligible for
explicit retention after the default 30-day window; nothing deletes it
automatically. Expired stages cannot attach, even before their bodies are purged.

## Message shape, kinds and confidence

A history item is the message envelope plus `seq` and optional purge fields.
Routed output has envelope kind `notice`; the turn's classification is
**`metadata.kind`**. A representative item is:

```json
{
  "seq":42,
  "id":"message-id",
  "from":"msg://session/local/session-id",
  "to":"msg://service/local/channel/ops",
  "kind":"notice",
  "channel":"ops",
  "thread_id":"session-id",
  "created_at":"2026-10-03T00:00:00Z",
  "delivered_at":null,
  "consumed_at":null,
  "content_type":"application/json",
  "payload":{"text":"The change is ready for review."},
  "metadata":{
    "session_id":"session-id",
    "turn_id":"turn-id",
    "kind":"final",
    "stop_reason":"end_turn",
    "confidence":"exact",
    "runtime":"codex",
    "logical_agent_id":"task-agent",
    "project_id":"project-id",
    "workstream_id":"workstream-id",
    "launch_id":"demo-launch",
    "launch_display_name":"demo-launch"
  }
}
```

Empty optional metadata values can occur. The router adds launch attribution;
`launch_display_name` currently falls back to `launch_id`. Threads group messages
by sender session within the channel. Runtime ids use registry primary ids:
`claude`, `codex`, `antigravity` (not `agy`), `opencode`, and ACP agent ids.

| Output kind | Consumer meaning |
|---|---|
| `final` | User-facing output from a completed turn; it is not proof that a task succeeded |
| `question` | Ended turn whose unresolved question signal still needs a response |
| `approval` | Ended turn whose unresolved refusal/approval signal still needs a response |
| `failure` | Failed turn; inspect its stop reason and text |
| `terminal` | Turn cut short by interruption/cancellation or process exit; partial text or a reason, bus excerpt only, never routed |

`stop_reason` describes how the turn ended. Normalized values include
`end_turn`, `max_tokens`, `tool_use`, `turn_limit`, `refusal`, `cancelled` and
`error`; a provider-specific reason can also pass through. Treat this as an
extensible string, not a closed enum or a task-success indicator.

Output `confidence` describes **text extraction**, not the certainty of the
question/approval classification. Exact terminal text or final-phase deltas are
`exact`; a fallback last text block is `heuristic`. Never interpret exact text as
an exact inference that the agent cannot proceed without approval. Failure and
terminal confidence is exact for runtime error text, heuristic for a synthesized
reason. Terminal stop reasons are `cancelled` or `error`; an interrupting reply
can produce one for the interrupted turn.

Capability `final_text_confidence` values are `exact`, `heuristic`, `none` and
`unknown`. Supported Claude, Codex, Antigravity and OpenCode run modes have an
exact baseline once the publisher is wired; ACP is heuristic; OpenCode serve
has no final-text source (`none`). Unsupported or unwired paths are `unknown`.
**Per-output confidence is authoritative** over this runtime baseline.

Question/approval limits (CW-20261002-0073): detection concerns an ended turn,
not a request still pending inside an open turn. A later successful tool call
can show a refusal was worked around; a text-only workaround can still be
classified question/approval. Antigravity refusals lack tool ids, so a worked
around refusal can still surface as approval. Claude AskUserQuestion and Codex
declined/requestUserInput detectors have protocol/code coverage. However,
headless Claude Code 2.1.288 (streaming-stdio) does not offer AskUserQuestion:
`question` does not surface for Claude as launched today despite capability
advertisement (CW-20261003-0070). `kinds_available` describes detector wiring,
not proof that the capture path can produce a signal. Claude approval through
permission denials is a separate path, unexercised under bypass; remaining live
signal acceptance evidence is pending. Approval text can contain a refused
command, path, URL or description, including inline credentials; redaction is
not guaranteed. Choose what your consumer exposes to its audience.

## Turn-output events and bridge migration

The canonical event is `session.turn_output`. Empty-text finals publish no event;
failure/terminal can carry empty text. An event for every turn is not guaranteed.
Tangent's Agent Turns bridge previously waited for `session.turn_waiting_input`,
which Tether has never emitted. Migrate the bridge to `session.turn_output` and
handle the message-id-versus-excerpt distinction.

```json
{
  "session_id":"session-id",
  "turn_id":"turn-id",
  "kind":"final",
  "stop_reason":"end_turn",
  "confidence":"exact",
  "runtime":"codex",
  "logical_agent_id":"task-agent",
  "project_id":"project-id",
  "workstream_id":"workstream-id",
  "message_id":"message-id"
}
```

The event's attribution fields are `logical_agent_id`, `project_id` and
`workstream_id`; an unassigned workstream is an empty string. Workstream
assignment is read at publication rather than frozen at launch. `launch_id` and
`launch_display_name` belong to the channel message's metadata, added by the
router, and are not fields on `session.turn_output`.

A selected kind on a routed session carries `message_id` after durable staging.
Other outputs carry a UTF-8-bounded excerpt of at most 4 KiB in `text`, with
`text_truncated: true` only when shortened (false is omitted), and no durable
message. Selected staging failures retain the full journal body for retry; an
unresolved route also keeps full output pending until the actual route is read. The event does not
mean channel attachment has completed: `GET /messages/{message_id}` returns 404
while staged. Consume channel history/SSE for committed publication; attachment
exposes the same id and body. An excerpt is not an independently stored full body.

`GET /events/stream?kind=session.turn_output&session_id=<id>&since_seq=<bus-seq>`
streams the event. SSE `data` wraps `scope`, `session_id` and `payload_json`.
Decode `data` as JSON, then decode the JSON **string** in `payload_json` to obtain
the payload above. Historical records likewise use `payload_json`, alongside
sequence, time and kind. Event-bus sequences and channel publication cursors differ. Bus excerpts of up
to 4 KiB are retained in event history for 90 days for every session, routed or
unrouted; this can expose text beyond the channel audience.

Persistence retries can publish later turns before earlier ones. Recovery scans
attach in staging-time order (message id breaks timestamp ties), but live events,
authorization denials and retry backoff can change publication order. Use
session/turn metadata to identify outputs; neither stream promises model-turn
order across retries. New producer events include `output_id`, binding the
session, turn, kind, native source identity and original body. Staging and event
replay use this stable identity; consumers should not deduplicate by text alone.
`fresh_conversation: true` reports loss of provider conversation continuity and
is omitted when false; staged message metadata retains the same annotation.

Before database work, output is committed to a private retry journal beside the
selected state database. Startup and periodic replay recover committed records
after a crash, worker expiry or a full pool. Known unrouted output still retains
only its bounded excerpt. Replay reads are bounded to 128 MiB per record and the
30-day routing window; oversized and expired records remain for operator
handling. A failed journal write can still prevent durable recovery. A repeated
identical Error suppressed by the reducer can produce neither a failure event
nor a routed failure message. These boundaries do not promise public exactly-once
delivery. See [runtime turn output](runtime-turn-output.md) for the full contract.

Attachment records `session.turn_routed` transactionally with publication. This
audit is available via
`GET /events?kind=session.turn_routed&session_id=<id>`, **not live bus/SSE fanout**;
its fields are `actor`, `publisher`, `session_id`, `turn_id`, `channel` and
`message_id`. The router is the audit actor and the sender session the publisher.
The internal router does not synthesize token scopes; future scope policies must
explicitly authorize it.

## Discover installed capabilities

`GET /routing/capabilities` describes the gateway. Add `?session_id=<id>` to
inspect that session's persisted runtime/mode; an unknown session returns 404.
Per-session responses also echo `session_id`. The response includes gateway
flags, `delivery: "next-turn"`, and a `runtimes`
map keyed by the registry ids above. Each runtime entry carries `route_supported`,
`reply_to_sender`, `interrupt`, `kinds_available` and `final_text_confidence`.

True means the path is wired: publication plus a running router for routing;
installed dispatcher plus available publisher kinds for replies; actual adapter
`cancel_turn` advertisement
**and** Tether's interrupt path for interrupt. Stopping a session is not evidence
that cancel-turn is supported. Question/approval availability requires that
runtime's installed detector and publication feed, not just a tagged library.

Gateway flags mean at least one advertised runtime supports the path and its
kinds are their union. Multiple configured modes for the same runtime id produce
an entry containing their common guarantees. Use the session query for decisions
about one sender; do not use another runtime's gateway-level support.

Installed detectors expose question and approval for Claude/Codex, approval for
Antigravity, and final/failure for other registered sources. Actual availability
also depends on the runtime mode and installed feed. A `none` or `unknown`
final-text source yields empty `kinds_available` and false `route_supported`.

## HTTP and SSE example

These commands use the default Unix socket and an asserted local caller in
observe mode. With verified authentication, use your configured bearer mechanism.
Under ENFORCE every route here requires a verified bearer; without it the daemon
returns 401. In observe/off mode channel calls require the asserted `as`;
`/routing/capabilities` and `/delivery` handlers require no assertion. Replace the
socket path for another deployment.

```bash
TETHER_SOCKET="$HOME/.tether/run/tetherd.sock"
TETHER_CALLER='msg://service/local/consumer-example'
curl --fail-with-body --unix-socket "$TETHER_SOCKET" --get \
  --data-urlencode "as=$TETHER_CALLER" http://localhost/channels
curl --fail-with-body --unix-socket "$TETHER_SOCKET" --get \
  --data-urlencode "as=$TETHER_CALLER" --data-urlencode 'last=20' \
  http://localhost/channels/ops/messages
curl --fail-with-body --unix-socket "$TETHER_SOCKET" \
  http://localhost/routing/capabilities
curl --fail-with-body --no-buffer --unix-socket "$TETHER_SOCKET" --get \
  --data-urlencode "as=$TETHER_CALLER" --data-urlencode 'since=0' \
  http://localhost/channels/ops/subscribe
```

After a disconnect, run the subscribe request again with the last processed
sequence in `since` or `Last-Event-ID`. Persist the cursor after your consumer's
side effect commits and deduplicate by message id/sequence. Curl does not manage
reconnects for you. Browser EventSource reconnects automatically and supplies
Last-Event-ID; its higher cursor wins over the original query.

## Go client example

Use go-tether-client **v0.10.0**, containing PR #9 (channels/capabilities) and
PR #10 (replies/delivery). Save this as `main.go`
in a Go module requiring that version,
then run `go run .`. It loads recent history, then subscribes from `NextSince`:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"

	tether "github.com/hollis-labs/go-tether-client"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	client, err := tether.New("", tether.WithSelfURN("msg://service/local/consumer-example"))
	if err != nil {
		log.Fatal(err)
	}
	channels, err := client.ListChannels(ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(channels)
	capabilities, err := client.RoutingCapabilities(ctx, "")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(capabilities)
	page, err := client.ChannelMessages(ctx, "ops", tether.ChannelMessagesOptions{Last: 20})
	if err != nil {
		log.Fatal(err)
	}
	show := func(m tether.ChannelMessage) { fmt.Printf("%d %s purged=%v\n", m.Seq, m.ID, m.Purged) }
	for _, m := range page.Messages {
		show(m)
	}
	since := page.NextSince
	messages, failures, err := client.SubscribeChannel(ctx, "ops", &since)
	if err != nil {
		log.Fatal(err)
	}
	for messages != nil || failures != nil {
		select {
		case m, ok := <-messages:
			if !ok {
				messages = nil
				continue
			}
			show(m)
			since = m.Seq // persist after processing in a durable consumer
		case err, ok := <-failures:
			if !ok {
				failures = nil
				continue
			}
			log.Printf("stream failed; resume after %d: %v", since, err)
		case <-ctx.Done():
			return
		}
	}
}
```

`SubscribeChannel` takes `*int64`: nil means live-only, a pointer to zero means
all history. It returns message and asynchronous error channels plus an initial
setup error. It **does not auto-reconnect**. Resume explicitly with the last
received `Seq` after processing it. SSE line buffers grow lazily to a 16 MiB
limit so JSON-escaped large turn text can be delivered. Delivery backpressure
blocks under the supplied context; cancel the context to stop the subscription.

## MCP example

Use an MCP client already connected to Tether; send these `tools/call` requests
on its initialized session. Tool names use underscores:

```json
{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"tether_channel_list","arguments":{"as":"msg://service/local/consumer-example","limit":20,"offset":0}}}
```

```json
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"tether_channel_read","arguments":{"as":"msg://service/local/consumer-example","name":"ops","last":20}}}
```

```json
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"tether_routing_get","arguments":{"session_id":"session-id"}}}
```

List pages return `next_offset` when more exist; read pages return `next_since`.
Use `since`/`limit` instead of `last` to continue. The calls are read-only thin
wrappers over the same services as HTTP, including identity and tombstones.
MCP tools cannot hold an SSE subscription; use HTTP or the Go client for streaming.
For ordinary replies, `tether_message_send` requires `from`, `to`, `kind` and
`message.write` scope. Set `to` to the sender session URN (omitting `to` is an
HTTP-only option), `in_reply_to` to the channel message id, and put the payload
in `payload_json` as encoded JSON:

```json
{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"tether_message_send","arguments":{"from":"msg://service/local/consumer-example","to":"msg://session/local/session-id","kind":"response","in_reply_to":"message-id","payload_json":"\"Approved\""}}}
```

The result is `{"ok":true,"message":<envelope>}`; `message.id` is the reply id.
It does **not** expose `routing_reply`, and the tool has no Idempotency-Key
parameter: retrying an MCP call can queue a second reply. Use a known published
parent; the compatibility fallback described below is not distinguishable by
that receipt alone. There is no dedicated interrupting-reply or delivery-read
MCP tool. Use HTTP/Go for interrupt and `/delivery`; MCP-only consumers can read
`routing.reply_*` outcomes with `tether_session_events` or `tether_events_history`.

## Plugin example: a Tangent docs consumer

A plugin owns its relevance and presentation policy. The following runnable
worker shows the consumer portion of a Tangent plugin: fetch recent `ops`
messages and call Tangent's public `hostclient` to enqueue documents. It is an
example integration, not a shipped plugin or automatic forwarding policy.
Use it as a callback in your separately packaged plugin, with the host-provided
context and `TANGENT_MCP_URL`. It imports public packages, not either app's internals.

Save as a separate `main.go`; require go-tether-client **v0.10.0** and Tangent
**v0.15.0 or later**, exporting `pkg/plugin/hostclient`. The plugin module itself
must require that client version; Tangent main pins v0.8.0. Set
`TANGENT_MCP_URL` to your local
Tangent MCP endpoint, then `go run .`. It creates docs inbox items; a repeated run
uses stable idempotency keys. Real plugins should page from a durable cursor and
apply their own audience policy before presenting text.

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	tether "github.com/hollis-labs/go-tether-client"
	"github.com/hollis-labs/tangent/pkg/plugin/hostclient"
)

func main() {
	ctx := context.Background()
	client, err := tether.New("", tether.WithSelfURN("msg://service/local/ops-docs-plugin"))
	if err != nil {
		log.Fatal(err)
	}
	host, err := hostclient.New()
	if err != nil {
		log.Fatal(err)
	}
	defer host.Close()
	page, err := client.ChannelMessages(ctx, "ops", tether.ChannelMessagesOptions{Last: 20})
	if err != nil {
		log.Fatal(err)
	}
	for _, m := range page.Messages {
		if m.Purged {
			continue
		}
		var payload struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(m.Payload, &payload); err != nil {
			continue // ordinary channel publications may have other payload shapes
		}
		if payload.Text == "" {
			continue
		}
		result, err := host.CallTool(ctx, "tangent.docs_enqueue", map[string]any{
			"contract_version": "1.0",
			"idempotency_key":  "ops-docs:" + m.ID,
			"source":           map[string]any{"agent_id": "ops-docs-plugin", "application_id": "ops-docs"},
			"title":            fmt.Sprintf("ops: %s", m.ID),
			"content_markdown": payload.Text,
			"requires_ack":     false,
		})
		if err != nil {
			log.Fatal(err)
		}
		if result.IsError {
			log.Fatalf("docs enqueue refused: %s", result.Content)
		}
		fmt.Println(string(result.Content))
	}
}
```

Docs inbox creation is this plugin's explicit choice. Another consumer could
render a chat, filter to failures, or record metrics without notifying anyone.
Host plugin registration and lifecycle remain the host's responsibility; channel
subscription does not register that plugin with Tether.

## Reply to the sender

Use the channel publication's id as the parent:

```bash
TETHER_SOCKET="$HOME/.tether/run/tetherd.sock"
MESSAGE_ID='replace-with-the-channel-message-id'
curl --fail-with-body --unix-socket "$TETHER_SOCKET" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: consumer-example:approval-1' \
  --data '{"body":"Approved; continue.","interrupt":false}' \
  "http://localhost/messages/$MESSAGE_ID/reply?as=msg%3A%2F%2Fservice%2Flocal%2Fconsumer-example"
```

The dedicated endpoint returns HTTP 202 with an acceptance receipt:

```json
{"reply_id":"reply-message-id","parent_id":"message-id","state":"queued","target_session_id":"sender-session-id"}
```

The parent identifies the sender; consumers do not choose another recipient or
reply to the channel. Tether queues the body as the sender's next turn, waiting
while busy. Consumers never submit that turn separately. Acceptance does not
prove delivery. `GET /messages/{reply_id}/delivery` reports `pending`, `queued`,
`delivering`, `delivered` or `undeliverable`; `pending` reserves an interrupting
reply while cancel is in flight. The record includes `reply_id`, `parent_id`,
`state`, optional `reason`/`detail`, `original_session_id`, `target_session_id`,
optional `delivered_to_session_id`, `interrupt_requested`, `attempts`, optional
`next_attempt_at`, `created_at`, `updated_at` and optional `settled_at`.
Detail is one line of at most 256 bytes; a process failure reports exit code or
signal rather than stderr.

Under identity **ENFORCE**, the daemon requires a verified bearer for every
route except `/health` and `/a2a` (including its subpaths). Anonymous reply and
delivery requests therefore return 401. Beyond authentication, reply-specific
authorization is not installed (CW-20261002-0116): any verified caller may reply
to any routed message and read any reply's delivery record. The `/delivery`
handler checks neither identity nor ownership itself. In observe/off mode the
reply requires a valid `as` (or compatibility envelope `from`) without a verified
principal; `/delivery` requires no identity assertion.

HTTP errors use `{"error":{"code":"...","message":"..."}}`.

Supply `Idempotency-Key` for retry-safe acceptance. A matching retry returns the
original reply with `duplicate: true`, its current stored state, and no repeated
interrupt; the earlier interrupt outcome is omitted on duplicate receipts. Keys
are scoped to parent id and actor. Changed body, interrupt flag or target within that scope
returns 409 `idempotency_conflict`.

| Refusal | Meaning |
|---|---|
| 404 `not_found` | Unknown parent/reply id; a staged parent remains hidden until attachment |
| 400 `reply_target_not_a_session` | Parent is not a canonical channel publication from a local session |
| 413 `payload_too_large` | Reply text exceeds 128 KiB or dedicated JSON request exceeds 1 MiB |
| 400 `invalid_request` | Blank/invalid UTF-8 body, or unknown field in dedicated reply JSON (only `body` and `interrupt` are accepted) |
| 409 `turn_feed_unavailable` | Running PTY target reports no turn lifecycle; nothing queued |
| 409 `interrupt_unsupported` | Runtime cannot cancel a turn; nothing queued |
| 409 `turn_not_yet_started` | Submitted turn has not started; nothing queued |
| 400 `reply_not_mailbox` | Mailbox operation aimed at a routing reply |
| 501 `not_implemented` | Service installed but dispatcher stopped; a nil reply service gives dedicated endpoint 404 and compatibility mailbox fallback |

`POST /messages` with `in_reply_to` naming a routed channel message returns
**201 with the stored reply envelope plus `routing_reply`**, rather than the
dedicated endpoint's 202 receipt. Envelope `id` equals receipt `reply_id`, and
`in_reply_to` names the parent. Omit `to` or set it to the sender session; another
recipient returns 400 `invalid_request`. This send also requires a valid `kind`. Text comes from a string payload, an
object's `body`/`text`/`message`, or otherwise the raw payload. Ordinary mailbox
replies, including messages that merely have session senders, keep their behavior.
If the parent is unknown, still staged/hidden, or the reply service is nil, this
compatibility path silently falls back to mailbox delivery: 201 without
`routing_reply`, no routed turn queued. Treat a 201 **without `routing_reply`** as
not a routed reply. Prefer the dedicated endpoint to detect that failure as 404
`not_found` (for an unknown/hidden parent or absent endpoint).

`POST /messages/notify` with a routed `in_reply_to` also queues the reply body:
201 in notify's shape with the stored `message`, `routing_reply`, and
`wake_attempted: false`. Notify requires both `from` and `to` before checking routing (omit-`to` works
only on `POST /messages`). For a recognized routed parent it ignores `wake`,
`wake_text` and `urgency`; no mailbox
copy or reminder turn is created. Interrupt needs the dedicated endpoint.
Cancel, consume, read, archive, claim, ack, nack, redrive and delete aimed at a
routing reply return 400 `reply_not_mailbox`; read its state from `/delivery`.

If the original session ended, Tether resolves its stable agent binding to a
running successor, never a newest-running guess. Queues are FIFO, including
backoff, except an interrupting reply is reserved before cancel and takes
priority over ordinary queued replies. A `handed_off` reason alone does not prove
delivery: handoff first requeues on the successor.

| Delivery reason | State / action |
|---|---|
| `handed_off` | First queued with retargeted `target_session_id`; delivered only after injection. Inspect `state` and `delivered_to_session_id` |
| `turn_failed` | Delivered: turn activity or subprocess exit was observed; failure is terminal and not retried |
| `session_ended_no_binding` | Undeliverable: no usable current actor binding |
| `bound_session_not_running` | Undeliverable: bound successor is not running |
| `pull_only_binding` | Undeliverable: binding cannot accept injected turns |
| `resolve_failed` | Undeliverable: binding resolution kept failing |
| `submit_failed` | Queued while retrying a submission the runtime did not take; undeliverable after five failed attempts |
| `no_turn_feed` | Undeliverable: queued reply reached a PTY target with no idle boundary |
| `daemon_restarted_during_delivery` | Undeliverable for a still-running target; a gone target requeues for binding resolution |
| `interrupt_unconfirmed` | Undeliverable: daemon stopped during interrupt reservation; resubmit with a **new** key |
| `body_purged` | Undeliverable: reply text was purged before delivery |
| `waiting_for_idle` | Queued: runtime rejected mid-turn input; wait for the turn to end |

`routing.reply_delivered` and `routing.reply_undeliverable` report outcomes on the
target session's event stream. Payload fields are `reply_id`, `parent_id`,
`state`, optional `reason`/`detail`, `original_session_id`, `target_session_id`,
optional `delivered_to_session_id`, `logical_agent_id` and `actor`; no reply text.
Event SSE uses the `payload_json` wrapper described above.

Only failures where the runtime says it took no turn are retried, such as failed
start/sandbox, missing login or a lost provider resume session. A subprocess
nonzero exit, **even a silent one**, settles as `delivered` / `turn_failed` after
one attempt and is never replayed. Tether cannot know whether it read the reply
before dying. Streaming runtimes report subsequent turn failure separately on
`session.turn_output`; delivered does not mean task success.

Known limitation: retry after `provider_session_lost` starts a **fresh provider
conversation**, without `--resume`. Consumers see plain `delivered`, `attempts: 2`,
with no reason marking the lost context. Restate the question in replies that
depend on it. For `interrupt_unconfirmed`, resubmit with a **new Idempotency-Key**:
the old key returns 202 `duplicate: true` on the undeliverable row and injects
nothing. Ambiguous injection after daemon restart is not replayed into a
still-running target.

With `interrupt: true`, Tether cancels the snapshot turn when cancel support is
wired, waits for it to end, then queues the reply as a new turn. It does not edit
the prompt or stop the session. Check the sender's session capabilities first.
A receipt reports `interrupt: "cancelled"`, or `no_turn_in_progress`,
`turn_superseded`, `session_not_running` when ordinary next-turn delivery was
accepted without cancellation. `session_ended` maps to `session_not_running` and
follows binding handoff or undeliverable resolution. `interrupt_timeout` is also
**accepted**: cancel was requested but the turn did not end within the wait bound;
the reply waits for eventual idle. Do not submit a new reply merely because that
wait timed out. Unsupported/not-yet-started refusals leave nothing queued.

The Go client exposes `Reply` and `ReplyDelivery`. This separate runnable example
uses the channel message id from the HTTP example. Set `MESSAGE_ID` and keep one
stable idempotency key for retries of this logical reply. Enable interrupt only
after checking the sender's capabilities. Neither method retries or polls.

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	tether "github.com/hollis-labs/go-tether-client"
)

func main() {
	ctx := context.Background()
	client, err := tether.New("", tether.WithSelfURN("msg://service/local/consumer-example"))
	if err != nil {
		log.Fatal(err)
	}
	receipt, err := client.Reply(ctx, os.Getenv("MESSAGE_ID"), "Approved; continue.", tether.ReplyOptions{
		Interrupt:      false,
		IdempotencyKey: "consumer-example:approval-1",
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(receipt)
	delivery, err := client.ReplyDelivery(ctx, receipt.ReplyID)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("state=%s reason=%s attempts=%d\n", delivery.State, delivery.Reason, delivery.Attempts)
}
```
