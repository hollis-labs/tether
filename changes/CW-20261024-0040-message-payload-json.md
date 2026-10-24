- Stop `tether_message_send` and `tether_message_notify` from silently dropping a
  `payload_json` that is not a string (CW-20261024-0040). A JSON object or array is
  now serialized and stored (with a warning logged), any other type is refused as
  `invalid_request` naming the argument, and an absent or null value behaves as
  before. Previously the message was stored with `payload: null` and the call
  returned `ok: true`, so recipients saw a blank body. The same drop was fixed
  client-side only in CW-20260521-0018. The argument description now says what is
  accepted.
