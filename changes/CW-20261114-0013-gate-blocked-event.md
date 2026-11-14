- `seat.gate_blocked` (CW-20261114-0013): when the Stop gate blocks a turn it reports the message ids to the hub
  (`tether_stop_gate_report` / `POST /messages/pending-replies/blocked`), which publishes
  `seat.gate_blocked {agent_urn, handle, message_ids, count}` on the seat's session, so a block is visible on the
  event bus. Best effort: a failed report never changes the gate's decision.
