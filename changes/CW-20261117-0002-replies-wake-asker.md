- Replies reach the asker (CW-20261117-0002): for a seat with mail triage on, a reply to
  mail the seat sent (`in_reply_to` names its message) now classifies as action
  (`reply_to_own`) whatever its kind, so untyped `response` mail wakes a hosted seat instead
  of waiting for a human prompt. Every triaged wake is coalesced: it lists the seat's other
  pending action and fyi mail, and the mail it names is settled (covered / surfaced) only
  once the turn is delivered. The default wake text now reads "Read and handle these. Reply
  to anything that asked you something. End silently only if nothing needs action." A wake
  refused because the session was busy is re-armed on the session's turn end and no longer
  gives up after 12 attempts; triaged seats keep no 6 h age bound. On first sweep after
  start the daemon backfills pending fyi replies-to-own to action (idempotent,
  `mail.triage_backfill`).
