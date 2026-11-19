# Terminal attachment API

With `daemon.terminal_attachments.enabled: true` (default off, CW-20261118-0012)
the daemon supplies `TerminalHost` and registers `/terminal-attachments`. It is
separate from `daemon.chat_wire.enabled`; either can be on alone, and with both
on they use one chat host, so a terminal controller and a chat controller
contend for one writer lease: a terminal takeover makes chat submits 403
`lease_required`, a chat takeover makes terminal input 409. A target is a
conversation ID or a session ID; a session that a relaunch superseded is 409
stale, and so is every attachment opened at the older generation. Only a PTY this
daemon runs is attachable (a worker session is 409 "runtime has no terminal").
Without the flag, the routes are not registered.

A new attachment observes by default; it
cannot send input or resize until it explicitly acquires a writer lease. The
host must resolve the complete current owner/operative/conversation/attempt/
generation binding and check it again at the PTY operation boundary. API agents
without a PTY are refused rather than given a synthetic terminal.

| Request | Result |
|---------|--------|
| `POST /terminal-attachments` `{"target":"<conversation-or-session>","role":"viewer"}` | 201 viewer attachment with an opaque ID and exact session/generation; `controller` role is requested explicitly |
| `GET /terminal-attachments/{id}/stream?since_offset=N` | PTY byte stream with retained-window and gap headers |
| `POST /terminal-attachments/{id}/lease` `{"takeover":false}` | Writer token and expiry; 409 while another current writer holds it |
| `POST /terminal-attachments/{id}/lease` `{"takeover":true}` | Explicitly revoke the previous writer and issue a new token |
| `POST /terminal-attachments/{id}/renew` | Extend the writer lease |
| `POST /terminal-attachments/{id}/input` | Write opaque PTY bytes, at most 1 MiB |
| `POST /terminal-attachments/{id}/resize` `{"rows":24,"cols":80}` | Resize the PTY |
| `DELETE /terminal-attachments/{id}` | Detach and revoke that attachment's writer lease |

The conversation host issues and validates the lease; this adapter does not
create a second writer authority. Input, resize and renew require
`X-Tether-Writer-Lease`. Observer control is
403; a stale binding is 409. Stream headers include `X-Tether-Attach-Cursor:
byte-offset`, oldest/next offsets, `X-Tether-Attach-Evicted`, and
`X-Tether-Terminal-Diagnostic: true`. A gap means the client must visibly mark
lost bytes. Raw terminal activity is diagnostic and is not itself a committed
chat event or approval. The host reconciles any action before it enters rich
chat. Existing direct `/sessions/{id}` PTY routes remain separate diagnostic
access and do not acquire a conversation writer lease.

`internal/terminalattach.ChatHost` connects this surface to the durable
`chathost.Host` in libs. The integrator supplies an authenticated target
resolver that maps both conversation IDs and explicit session targets to the
current binding and exact PTY session. The resolver reads the live PTY mapping
independently of the SDK host, because input and resize call it under the SDK
host's lease lock. The PTY interface must deliver input and resize synchronously;
queued transports must check the same lease again at delivery. The daemon's resolver
is `chatwire.TerminalTargets` over `conversation.Resolver`. The surface stays
opt-in until the direct input policy for managed sessions lands
(CW-20261116-0019).
