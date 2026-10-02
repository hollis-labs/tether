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

This guide describes the merged interfaces and labels remaining integration
**planned**. A merge does not establish that your daemon is deployed or that a
client tag exists. Check the deployed capabilities before using optional paths.

| Surface | State when this guide was authored |
|---|---|
| Route configuration | Merged, CW-20261002-0063 |
| Named channel history and SSE | Merged, CW-20261002-0069, PR #133 |
| Capability endpoint and MCP consumer tools | Merged, CW-20261002-0066, PR #137 |
| Go channel/capability client | Merged in go-tether-client PR #9; release tag follows daemon deployment |
| `session.turn_output` publisher | Planned integration, CW-20261002-0062, PR #136 |
| Turn-output attachment to channels | Planned integration, CW-20261002-0064 |
| Reply-to-sender delivery | Planned contract, CW-20261002-0065 |
| Interrupt before next-turn delivery | Planned integration, CW-20261002-0067 |
| Question/approval detection | Libraries tagged, CW-20261002-0073; daemon feed wiring still required |

On main immediately after PR #137, `route_supported`, `reply_to_sender` and
`interrupt` are false, `kinds_available` is empty, and `final_text_confidence` is
`unknown`. These are honest missing-wiring results. Channels can already receive
ordinary publications independently of the planned session router.

The [API reference](api/README.md) specifies the HTTP interfaces. The
[launch guide](caller-launched-sessions.md) explains catalog and override inputs.

## Enable routing on a launch

A route contains a channel **name** and selected output kinds:

```json
{"route":{"channel":"ops","kinds":["question","approval"]}}
```

Put that `route` field on a catalog agent or launch, supply it in an override JSON file,
or pass it in the session-create API. Each explicit route block replaces the
lower-precedence route, rather than merging its kinds. API route overrides the
JSON override route, which overrides the selected agent's route. The resolved
route is persisted with the launch; later catalog edits do not change it.

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
asserted `as` URN. The default channel authorization hooks operate in observe
mode; this does not establish future grants or enforcement policy.

| Read | Behavior |
|---|---|
| `GET /channels?as=<caller>` | Published names and derived addresses, sorted by name |
| `GET /channels/{name}/messages?as=<caller>&since=0&limit=100` | Messages strictly after `since`, oldest first; returns `next_since` |
| `GET /channels/{name}/messages?as=<caller>&last=20` | Latest 20, returned oldest first; excludes `since` and `limit` |
| `GET /channels/{name}/subscribe?as=<caller>&since=0` | SSE replay, then live publications |

History `limit` defaults to 100 and is capped at 1000; `last` accepts 1–1000.
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

Planned CW-20261002-0062/0064: selected turn output is stored once as a staged
message and attached to the channel using the same message id. Staging is hidden
from mailbox and sysop inboxes. A never-attached staged body becomes eligible for
explicit retention after the default 30-day window; nothing deletes it
automatically.

## Message shape, kinds and confidence

A history item is the message envelope plus `seq` and optional purge fields.
Planned CW-20261002-0062/0064 routed output has envelope kind `notice`; the turn's
classification is **`metadata.kind`**. A representative item is:

```json
{
  "seq":42,
  "id":"message-id",
  "from":"msg://session/local/session-id",
  "to":"msg://service/local/channel/ops",
  "kind":"notice",
  "channel":"ops",
  "thread_id":"session-id",
  "payload":{"text":"The change is ready for review."},
  "metadata":{
    "session_id":"session-id",
    "turn_id":"turn-id",
    "kind":"final",
    "stop_reason":"completed",
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
| `terminal` | Turn ended without routable user-facing output; bus only |

Output `confidence` describes **text extraction**, not the certainty of the
question/approval classification. Exact terminal text or final-phase deltas are
`exact`; a fallback last text block is `heuristic`. Never interpret exact text as
an exact inference that the agent cannot proceed without approval.

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
declined/requestUserInput paths have protocol/code coverage; captured live
acceptance evidence remains pending. Approval text can contain a refused
command, path, URL or description, including inline credentials; redaction is
not guaranteed. Choose what your consumer exposes to its audience.

## Turn-output events and bridge migration

**Planned CW-20261002-0062:** the canonical event is `session.turn_output`.
Tangent's Agent Turns bridge previously waited for `session.turn_waiting_input`,
which Tether has never emitted. Migrate the bridge to `session.turn_output` and
handle the message-id-versus-excerpt distinction.

```json
{
  "session_id":"session-id",
  "turn_id":"turn-id",
  "kind":"final",
  "stop_reason":"completed",
  "confidence":"exact",
  "runtime":"codex",
  "logical_agent_id":"task-agent",
  "project_id":"project-id",
  "workstream_id":"workstream-id",
  "message_id":"message-id"
}
```

A selected kind on a routed session carries `message_id` after durable staging.
Other outputs carry a UTF-8-bounded excerpt of at most 4 KiB in `text`, with
`text_truncated`, and no durable message. A staging failure also falls back to
that excerpt. The event does not mean channel attachment has already completed;
consume channel history/SSE for published messages. Do not treat excerpt text as
an independently stored full body.

`GET /events/stream?kind=session.turn_output&session_id=<id>&since_seq=<bus-seq>`
streams that event; its SSE `data` is the payload above. Historical event records
use `payload_json` for the payload string. Its event-bus sequence is not a
channel's publication `seq`.

## Discover installed capabilities

`GET /routing/capabilities` describes the gateway. Add `?session_id=<id>` to
inspect that session's persisted runtime/mode; an unknown session returns 404.
The response includes gateway flags, `delivery: "next-turn"`, and a `runtimes`
map keyed by the registry ids above. Each runtime entry carries `route_supported`,
`reply_to_sender`, `interrupt`, `kinds_available` and `final_text_confidence`.

True means the path is wired: publication plus a running router for routing;
installed dispatcher for replies; actual adapter `cancel_turn` advertisement
**and** Tether's interrupt path for interrupt. Stopping a session is not evidence
that cancel-turn is supported. Question/approval availability requires that
runtime's installed detector and publication feed, not just a tagged library.

Gateway flags mean at least one advertised runtime supports the path and its
kinds are their union. Multiple configured modes for the same runtime id produce
an entry containing their common guarantees. Use the session query for decisions
about one sender; do not use another runtime's gateway-level support.

## HTTP and SSE example

These commands use the default Unix socket and an asserted local caller in
observe mode. With verified authentication, use your configured bearer mechanism;
identity policy is still applied. Replace the socket path for another deployment.

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

Use a go-tether-client release containing PR #9, or its merged source until that
release is tagged. Save this as `main.go` in a Go module requiring that version,
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
There is no merged routing-reply tool to call yet (CW-20261002-0065).

## Plugin example: a Tangent docs consumer

A plugin owns its relevance and presentation policy. The following runnable
worker shows the consumer portion of a Tangent plugin: fetch recent `ops`
messages and call Tangent's public `hostclient` to enqueue documents. It is an
example integration, not a shipped plugin or automatic forwarding policy.
Use it as a callback in your separately packaged plugin, with the host-provided
context and `TANGENT_MCP_URL`. It imports public packages, not either app's internals.

Save as a separate `main.go`; require the channel-capable Go client and a Tangent
version exporting `pkg/plugin/hostclient`. Set `TANGENT_MCP_URL` to your local
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
			log.Fatal(err)
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

**Planned integration CW-20261002-0065** (implemented on its task branch), with
optional interrupt from CW-20261002-0067:

```bash
TETHER_SOCKET="$HOME/.tether/run/tetherd.sock"
MESSAGE_ID='replace-with-the-channel-message-id'
curl --fail-with-body --unix-socket "$TETHER_SOCKET" \
  -H 'Content-Type: application/json' \
  --data '{"body":"Approved; continue.","interrupt":false}' \
  "http://localhost/messages/$MESSAGE_ID/reply?as=msg%3A%2F%2Fservice%2Flocal%2Fconsumer-example"
```

The acceptance response is HTTP 202:

```json
{"reply_id":"reply-message-id","parent_id":"message-id","state":"queued","target_session_id":"sender-session-id"}
```

The parent message identifies the sender; the consumer does not choose another
recipient or reply to the channel. Tether queues the body and submits it as the
sender's next turn, waiting while that session is busy. Acceptance is not proof
of delivery. Inspect `GET /messages/{reply_id}/delivery` for queued/delivering/
delivered/undeliverable state and a reason. The delivery record also carries
`parent_id`, `original_session_id`, `target_session_id`, optional
`delivered_to_session_id`, `interrupt_requested`, attempt count, scheduling and
settlement timestamps, and optional error `detail`.

Supply an optional `Idempotency-Key` header for retry-safe acceptance. A matching
retry returns the original receipt with `duplicate: true` and does not interrupt
again; reusing the key for different input returns 409 `idempotency_conflict`.
Bodies are limited to 128 KiB (413 `payload_too_large`). Unknown ids return 404
`not_found`; a non-session sender returns 400 `reply_target_not_a_session`.
The installed service returns 501 `not_implemented` when its dispatcher is not
running; an older daemon may have no endpoint at all.

`POST /messages` with `in_reply_to` naming a routed channel message also takes
this reply path and returns the same 202 receipt instead of an envelope. If `to`
is supplied, it must name the sender session. Other mailbox replies keep their
existing behavior.

If the original session ended, Tether can resolve its stable agent binding to a
running successor. No binding, a non-running successor, a pull-only binding,
resolution failure, or submission failure can make delivery undeliverable.
An ambiguous in-flight delivery across restart is reported rather than silently
promising exactly-once injection. Show the state to the user and make retries
explicit.

| Delivery reason | State / action |
|---|---|
| `handed_off` | Delivered to the actor's bound successor; inspect `delivered_to_session_id` |
| `session_ended_no_binding` | Undeliverable: no current actor binding |
| `bound_session_not_running` | Undeliverable: bound successor is not running |
| `pull_only_binding` | Undeliverable: binding cannot accept injected turns |
| `resolve_failed` | Undeliverable: binding resolution kept failing |
| `submit_failed` | Queued while retrying; undeliverable after five failed submissions; inspect `detail` |
| `daemon_restarted_during_delivery` | Undeliverable: ambiguous injection is not replayed into the still-running session |
| `body_purged` | Undeliverable: reply text was purged before delivery |
| `waiting_for_idle` | Queued: current turn has not ended |

`routing.reply_delivered` and `routing.reply_undeliverable` events report outcomes
on the target session's event stream. Their payloads contain attribution and
state, not reply text.

With `interrupt: true`, Tether cancels the current turn only when actual
cancel-turn support is wired, waits for its end, then submits the reply as a new
turn. It does not edit the running prompt or stop the whole session. Planned 409
codes `interrupt_unsupported` and `turn_not_yet_started` reject acceptance; decide
whether to retry without interrupt or after the turn starts. Check the sender's
session capabilities before offering interrupt. A receipt can report
`interrupt: "cancelled"`, or `no_turn_in_progress`, `turn_superseded` or
`session_not_running` when nothing was cancelled and ordinary next-turn delivery
was accepted.

The client currently leaves a `ReplyOptions{Interrupt}` seam; **`Client.Reply`
is not available yet**. Use the planned HTTP contract only after its endpoint is
installed. The old mailbox/notify APIs are not substitutes for channel replies.
