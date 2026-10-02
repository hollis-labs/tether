# Session routing: a consumer guide

Tether captures a managed session's user-facing turn output and, when the
launch opts in, publishes selected kinds into a named channel. Consumers
subscribe to channels and choose what to display or act on. Tether does not
register consumers, choose their relevance rules, or decide whether Chrispian
needs to see a message.

Routing is off by default. Routes target channels; replies always return to
the session that sent the message. Tether owns queued next-turn delivery.
Interrupt is optional and must be supported by the actual runtime.

## Availability

This guide separates merged interfaces from planned integration. Channel
history/SSE and route configuration are merged. Turn-output publishing
(CW-20261002-0062), routing into channels (CW-20261002-0064), reply delivery
(CW-20261002-0065), interrupt integration (CW-20261002-0067), and capability/MCP
consumer surfaces (CW-20261002-0066) are documented below with their task ids.
The question/approval libraries are tagged (CW-20261002-0073); tagged library
support alone does not prove the daemon has wired their feeds. Check the
capabilities endpoint on the deployed daemon before using optional paths.

The HTTP [API reference](api/README.md) carries the endpoint details. A merged
change may still need a daemon deployment or client-library release.

## Channels

The stable handle is a case-sensitive name matching
`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`. Its address is derived from that name:
`msg://service/local/channel/<name>`. API responses carry both name and address;
paths and route configuration use the name.

No membership is required to read history or subscribe. Only publications
addressed to the canonical channel address enter public history; a private
mailbox's matching channel label does not make it public. Reading channel
history does not consume, acknowledge, archive or mark it read.

Messages have durable publication sequence cursors independent of timestamps.
History reads are oldest first, with exclusive `since` and `next_since` for
pagination. `last=N` loads recent messages directly, also oldest first.

Bodies and metadata follow the explicit audited message purge policy. Structural
rows and cursors remain; `purged: true` and `purged_at` identify tombstones.
Nothing automatically deletes a message because a subscriber read it.
