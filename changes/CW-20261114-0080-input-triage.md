- Turn outputs carry the triage class of the mail that woke the seat (CW-20261114-0080): routed-message metadata
  `input_triage` (comma-joined) and `session.turn_output` `input_triage` (array) give `human`, `action`, `fyi` or
  `read` for a bound `mail_wake` input, read from `wake_triage` by the input's waking message, in `input_ids` order,
  under the same proven-binding rule as `input_sources`; `""` for an input that is not a mail wake or was not triaged,
  and omitted when every position is empty. Tachyon never folds a reply to a `human` wake. New fixtures
  `unknown_mail_wake_action.json` and `unknown_mail_wake_human.json`.
