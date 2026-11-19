- Conversation-to-session mapping (CW-20261118-0008, docs/conversations.md): the daemon now records which
  session runs each seat's conversation. The first session for a seat (any session with a logical agent) mints an
  opaque ULID conversation at generation 1; every later session for that seat (fresh launch, relaunch/resume,
  team replacement) moves it, raising generation by exactly 1, in the transaction that writes the session.
  - Migration 0107 adds `conversations` and the append-only `conversation_generations`; no backfill.
  - New read-only resolver `internal/conversation` (by conversation ID and by session ID); it reads only the
    store and never imports the chat host. Nothing calls it yet: no route or handler changed.
