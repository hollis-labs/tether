- Serve `/terminal-attachments` behind `daemon.terminal_attachments.enabled` (CW-20261118-0012, default off, separate
  from `daemon.chat_wire.enabled`): the daemon opens one chat host for the chat wire and terminal attachments, so a
  terminal controller and a chat controller contend for one writer lease (a terminal takeover makes chat submits 403
  `lease_required`, a chat takeover makes terminal input 409). Targets are a conversation or session ID, resolved
  through `conversation.Resolver` (new `Resolve`, a superseded session is stale) and never through the chat host; an
  attachment from before a relaunch is refused 409 stale. Only PTYs this daemon runs are attachable. With the flag off
  no route, mount or behaviour changes. The legacy `/sessions/{id}/input` and `/resize` routes still bypass the lease
  (CW-20261116-0019).
