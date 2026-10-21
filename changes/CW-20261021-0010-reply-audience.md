- Record who asked for each routed agent output (CW-20261021-0010, slice 1): every
  input handed to a session (chat send, `/turn`, MCP send-turn, mail wake, routing
  reply, the codex prime, and the text that starts a chat session) is recorded in the
  new `turn_inputs` table (migration 0097) with a sender class (`operator`, `agent`,
  `system`, `unknown`) derived from the verified principal, or for mail from the verified principal
  recorded with the message (never its claimed From, which the shared operator
  credential can set to anything); the shared operator credential is `unknown` on every
  route, so `operator` means a paired device with an interactive scope, never from prose or timestamps. The oldest accepted input is bound to the
  turn that answers it, and the routed message and `session.turn_output` gain
  `audience`, `audience_basis`, `input_ids` and `input_classes`; any doubt (queued or
  possibly merged inputs, an unsettled submission, an unverified sender) is `unknown`,
  historical outputs carry nothing, and sender URNs stay in the database. Rows follow
  the events retention window. See `docs/reply-audience.md`.
