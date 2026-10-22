- Mark Tether-managed sessions with `TETHER_HOSTED_SESSION=<session id>` in their
  process environment (CW-20261021-0029, step 2): set on the daemon's own launches and
  on worker-hosted launches (through the launch spec, so workers need no upgrade). A
  hosted session is woken by the hub, so a mail plugin that sees this variable must not
  also inject the mailbox; it is the reliable hosted-versus-local signal the
  tether-mail watchers use. Not a credential, carries no authority.
