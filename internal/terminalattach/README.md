# Terminal attachment boundary

`Controller` and `ChatHost` provide an opt-in, observe-first PTY attachment
surface. `ChatHost` delegates attachment IDs, writer leases, takeover, renew,
and input/resize admission to the durable SDK `chathost.Host`. It compares the
full current binding and exact PTY session at delivery. Raw PTY output is
diagnostic; its byte offset is separate from committed rich-chat cursors.

Single-writer arbitration holds **only for clients using
`/terminal-attachments`**. The existing `/sessions/{id}/input` and
`/sessions/{id}/resize` routes bypass this lease, even while another UI writer
holds it. `Deps.TerminalHost` is nil by default, so the new route is off and
existing Tether input/resize behavior is unchanged. The daemon sets it with
`daemon.terminal_attachments.enabled` (CW-20261118-0012) from
`chatwire.NewTerminalHost`, over the chat wire's chat host, with
`conversation.Resolver` as the target resolver. CW-20261116-0019 owns the
daemon conversation-to-PTY mapping and the legacy-route policy before
activation. Do not present the optional surface as an exclusive writer gate
for all Tether clients until that work lands.

The integrator must supply an authenticated `TargetResolver` for both
conversation and explicit session targets. It must use a live mapping
independent of the SDK host because input and resize call it while the SDK
lease lock is held. PTY input/resize must finish synchronously; queued
delivery requires a fresh lease check when delivered.
