- Turn outputs say which route their input arrived by (CW-20261114-0061): routed-message metadata `input_sources`
  (comma-joined) and `session.turn_output` `input_sources` (array), in `input_ids` order, from the `turn_inputs.source`
  vocabulary (`chat_send`, `session_turn`, `mcp_send_turn`, `mail_wake`, `routing_reply`, `system`). Present whenever
  the binding is proven, including a mail wake whose sender class is unknown; absent for a queued or unsettled binding.
  Tachyon uses it to fold mail-wake turns. New fixture `docs/fixtures/reply-audience/unknown_mail_wake.json`.
