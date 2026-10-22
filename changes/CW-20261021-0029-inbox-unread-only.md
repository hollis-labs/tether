- A live recipient session's inbox pull is now unread-only (CW-20261021-0029): the
  pull returned every undelivered message, read or not, and consumed what it returned,
  so a message the agent had already read came back as new and was consumed again. Read
  but undelivered messages are settled instead (delivered_at stamped, delivery closed as
  consumed) and never come back. Operator and CLI pulls, and a session pulling another
  mailbox, are unchanged. No schema change.
