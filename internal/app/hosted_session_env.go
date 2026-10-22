package app

// HostedSessionEnv names the variable every Tether-managed session's process gets,
// holding its session id (CW-20261021-0029). A session that has it is woken by the
// hub (the mailbox wake turn), so a mail plugin running inside it would deliver the
// same message a second time and must stay out. It is set on both launch paths: the
// daemon's own runtimes and worker-hosted sessions (through the launch spec, so a
// worker needs no upgrade). It is not a credential and carries no authority.
const HostedSessionEnv = "TETHER_HOSTED_SESSION"
