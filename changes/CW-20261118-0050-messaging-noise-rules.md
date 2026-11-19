- Messaging noise rules (CW-20261118-0050): typed mail with `attention: none` never wakes a
  seat, with or without `mail_triage` (it stays unread and rides along on the next real
  wake); a pure acknowledgement reply ("Noted. No reply needed.", "Thanks") to an update is
  refused at `POST /messages/post` with a message telling the sender to send nothing
  (`catalog.defaults.mail_triage.ack_filter: false` turns it off); the wake text tells an
  agent with nothing to do to end the turn without producing any message.
