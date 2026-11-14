- Hub mail triage trusts `from` only when the hub verified it (CW-20261114-0125). In observe mode a claimed `from`
  that did not verify is still stored; triage now judges such mail by the principal that actually sent it
  (`from_principal`), or, with none, as an agent's. An agent claiming `from: msg://user/local/operator` is no longer
  triaged human/`from_operator`, and is not named @chrispian in `mail.triaged` or the FYI line. `mail.triaged` gains
  `from_verification`.
