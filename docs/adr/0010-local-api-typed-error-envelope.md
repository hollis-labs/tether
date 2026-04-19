# ADR 0010: Local API — Typed `{error:{code,message}}` Envelope

**Status:** Accepted — 2026-04-19
**Context:** Sprint v002-05 (Local API), task T-v002-s05-01a
**Deciders:** agent-mux v0.0.2 execution session

## Context

Pre-Sprint-5 handlers returned errors ad-hoc:

```json
{"error": "session not found"}
```

or occasionally a plain `string`. Clients had no way to branch on error
*class* (a 404 could be "session not found" or "route not registered",
and the body was the only disambiguator). When Sprint v002-05 started
wiring multiple external consumers (CLI, Nanite, Clockwork), the
ad-hoc shape failed two tests: it couldn't express a programmatic
error code, and it couldn't carry a human-readable message in the same
shape reliably.

## Decision

Every error response is a JSON object of shape:

```json
{
  "error": {
    "code": "<stable_snake_code>",
    "message": "<human readable>"
  }
}
```

Codes (defined in `internal/api/errors.go`):

- `invalid_request` — 400. Malformed body, bad query params, validation
  failure.
- `not_found` — 404. Target resource does not exist.
- `method_not_allowed` — 405. Route exists but not for this method.
- `payload_too_large` — 413. Request body exceeded the per-endpoint cap.
- `conflict` — 409. State-machine violation (e.g., launching a session
  that is not in `created` state).
- `not_implemented` — 501. Endpoint intentionally stubbed (today:
  `POST /logical-agents/{id}/resume` pending v0.0.3 Sprint v003-04).
- `internal_error` — 500. Unexpected server fault.

HTTP status code is set alongside; the envelope is the only body shape
clients parse.

## Alternatives Considered

- **RFC 7807 (Problem Details for HTTP APIs).** Rejected as overkill
  for a local-only API. The `type: <uri>` discipline is valuable for
  public APIs with external tool ecosystems; agent-mux's consumers
  are all in-portfolio.
- **Error code as HTTP header + plain body.** Rejected: JSON bodies
  are already the norm; a second encoding for errors splits client
  parsers.
- **Just use HTTP status codes.** Rejected: status codes are too coarse
  (e.g., 400 covers "malformed JSON" *and* "missing required field"
  *and* "limit out of range"). Clients want finer-grained branching.

## Consequences

- All handlers funnel through `writeError(w, http.StatusX, "code",
  "message")`. Mistakes (status without a code, code without a status)
  are caught by review + typed call signatures.
- CLI formats the envelope for humans; external clients parse it.
- The old single-string body shape was removed wholesale — pre-launch,
  no compat shim.
- Adding a new code is free (just define the constant). Changing an
  existing code is a breaking change that needs a new ADR.
- Related ADRs: [0009](0009-local-api-create-launch-split.md),
  [0011](0011-local-api-attach-transport.md).
