- Typed messages (CW-20261114-0013): `tether_message_post` / `POST /messages/post` sends a validated envelope
  (`question`, `update`, `handoff`, `escalation`, `approval_request`, `decision`, `message`) to seats by `@handle`; the
  server stamps the sender, resolves every handle and refuses a malformed envelope or an unknown handle with the field
  and the reason. `message.*` lifecycle events carry `envelope_kind`, `attention`, `awaiting`, `intent` and `re`.
  `tether_pending_replies` / `GET /messages/pending-replies` lists typed messages waiting on the caller. Posts take an
  optional `urgency`. Registry bootstrap gives each catalog agent the `@handle` of its new catalog `handle:` key, else its
  id (substrate `handle`); `tether_handle_set` / `POST /registry/handles` (operator) gives a non-catalog agent one. See
  `docs/typed-messages.md`.
- Seat status (CW-20261114-0013, folds the finger card CW-20261030-0179 v1): `tether_status_set` / `POST /status`
  declares available, working, waiting-decision, waiting-agent, blocked (with an owner), paused or offline, with reason,
  ref and expiry; the operator or a manager may set another seat's. `tether_status_get` / `GET /status/{@handle}` is the
  card: the declared state beside the live session, health, work state, last activity, unread mail and pending replies.
  A change emits `seat.status`. Migration 0100 adds the `seat_status` table.
