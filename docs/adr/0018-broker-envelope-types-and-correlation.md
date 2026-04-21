# ADR 0018 — Broker Envelope Types and Correlation-ID Scheme

**Status:** accepted
**Date:** 2026-04-21
**Supersedes:** —
**Superseded by:** —

## Context

v0.0.2 shipped `broker_envelopes` with a `message_type TEXT` column and a `correlation_id TEXT` column, but accepted any string and assigned no semantics. The multiplexor use-case (context-pack §04 use case 3) requires structured request/reply exchanges between agent sessions: a sender posts a `request` and blocks (or polls) until a correlated `response` arrives.

Without a pinned type enum:
- Clients use ad-hoc strings (`"req"`, `"msg"`, `"reply"`) and the broker can't route or validate.
- Correlation IDs are caller-assigned, risking collisions and making server-side blocking unimplementable.

## Decision

### Message type enum

Six types are valid for v0.0.3+:

| Type | Semantics |
|------|-----------|
| `request` | Initiates a request/reply exchange. Server assigns correlation_id. |
| `response` | Reply to a request. Must carry the request's correlation_id. |
| `notice` | One-way informational. No reply expected or required. |
| `escalation` | Signals human (or high-priority agent) attention needed. |
| `handoff` | Cross-logical-agent session transfer (distinct from same-agent handoff in ADR 0015). |
| `status_update` | Periodic agent status report. |

The API layer rejects any `message_type` not in this set with HTTP 400 `invalid_request`. Empty `message_type` is allowed (for backwards compat with v0.0.2 envelopes) and treated as untyped.

### Correlation-ID scheme

**For `request` envelopes:** the server generates a UUIDv7 `correlation_id` and returns it in the response body. Callers MUST NOT set `correlation_id` on a request — any client-supplied value is overwritten. UUIDv7 is used because it is time-sortable, allowing correlation lookups by creation order without a secondary index.

**For `response` envelopes:** `correlation_id` is required and must reference the originating request's `id` (not the request's `correlation_id`). The server validates this field is non-empty at write time; it does not currently validate that the referenced request exists (deferred to the dispatcher in Sprint v003-05 T-02).

**For all other types:** `correlation_id` is optional and caller-controlled.

### Implementation points

- Validation lives in `broker.Service.CreateEnvelope` (not only in the HTTP handler) so any future caller (internal or external) gets the same semantics.
- The API handler calls `IsValidMessageType` before constructing the `Envelope` struct, and re-writes `correlation_id` for requests.
- `broker.ValidMessageTypes()` returns the stable list used in error messages and documentation.

## Rationale

**Why server-assign the correlation_id for requests?** The blocking-wait implementation (Sprint T-02) subscribes to the event bus using the correlation_id as the lookup key. If clients could assign arbitrary IDs, a malicious or buggy caller could collide with an in-flight wait. Server assignment prevents that without requiring a registry check at write time.

**Why validate in Service, not just the HTTP handler?** The broker service is used by internal callers (checkpoint events, future mailbox workers) that don't go through the HTTP handler. Centralising validation in the service prevents logic drift.

**Why allow empty message_type?** v0.0.2 rows in production have empty `message_type`. Rejecting them on read would break the list endpoint. On write, empty is allowed and the envelope is treated as untyped; no special routing or wait semantics apply.

**Why not `reply` instead of `response`?** `response` is the HTTP-conventions term and matches OpenAPI vocabulary. `reply` is colloquial and was used inconsistently in v0.0.2 test fixtures. Renaming on the schema boundary (before any external consumer lands) has zero migration cost.

## Consequences

- `broker.IsValidMessageType`, `broker.ValidMessageTypes`, and the six constants live in `internal/broker/types.go`.
- Existing `broker.Service.CreateEnvelope` callers that pass unknown `message_type` values will receive errors after this change. Known callers (API handler, test fixtures) updated in the same commit.
- The API handler now always overwrites `correlation_id` for `request` envelopes. Clients that were setting it manually will see their value silently replaced.
- Sprint v003-05 T-02's dispatcher can rely on server-assigned UUIDv7 correlation_ids.

## Follow-ups

- T-02: validate that a `response.correlation_id` references an existing `request` that has not yet been consumed. Deferred — requires the dispatcher's in-memory request registry to be live first.
- Per-host network ACLs for broker recipients — deferred.
- `handoff` type routing (connect to a human queue or Clockwork) — deferred to v0.1.
