- Add explicit many-to-many registry project URN associations to workstreams
  across storage, HTTP, CLI, MCP and client contracts (CW-20261018-0009), with
  exact-membership cursor-bound filtering and seed-only ensure behavior.
  Existing rows remain unassociated; workflow and parent semantics stay opaque.
