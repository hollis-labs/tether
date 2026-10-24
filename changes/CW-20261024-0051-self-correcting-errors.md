- Make argument errors teach the contract (CW-20261024-0051). When a tool call fails
  and the arguments sent do not fit that tool's own schema, the gateway appends a
  `gateway_hint` content block: the unknown names, a `did_you_mean` mapping to the
  declared name (edit distance, shared tokens, and a small generic synonym set such
  as body/content and query/search), the missing and required names, the accepted
  names when there are 15 or fewer, and a minimal example. It is derived from the
  schema and the arguments only, attached only to a failed call, and never changes a
  successful one. Schema-detail discovery items carry an `example`. On the message
  tools, `kind` is now a real enum, a non-JSON `payload_json` string is refused as
  `invalid_request` (it used to surface as `internal_error`), and
  `tether_message_reply` on ordinary mail returns a typed redirect to
  `tether_message_send` (`helpTool`, `nextStep`).
