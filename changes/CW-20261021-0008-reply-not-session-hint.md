- Make the `reply_target_not_a_session` refusal actionable: the 400 message, the
  `tether_message_reply` tool description and the docs now say that a reply only
  answers a message a session published to a channel, and that an ordinary
  mailbox message (from a user, device or agent address) is answered with
  `tether_message_send` to its sender and `in_reply_to=<message_id>` (CW-20261021-0008). Text only;
  the rule, the code and the status are unchanged.
