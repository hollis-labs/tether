- Stop `tether_message_send` and `tether_message_notify` from silently dropping
  message bodies (CW-20261024-0040). A `payload_json` sent as a JSON object or array
  is now serialized and stored (with a warning logged) and any other non-string
  type is refused as `invalid_request`. An argument the tool does not declare, such
  as a top-level `subject` or `body`, is now refused by name with a hint to put it
  inside `payload_json`, and the accepted arguments are listed. `_traceparent` and
  `_tracestate` are still ignored. Both shapes used to be accepted, stored with
  `payload: null` and answered `ok: true`, so recipients saw a blank body. The same
  drop was fixed client-side only in CW-20260521-0018. Callers that pass an
  argument these tools do not declare (for example `no_wake` to `tether_message_send`)
  now get an error instead of the argument being ignored.
