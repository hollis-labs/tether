# AI Gateway Foundation Follow-up

Date: 2026-05-24

## What landed

- Concrete design/spec for Tether's AI gateway in
  `planning/docs/specs/ai-gateway-spec.md`.
- New `internal/llm` foundation package with:
  - normalized request/response types
  - middleware contract
  - middleware chain tests
- New `internal/modelcatalog` wrapper over `go-modelsdev` with:
  - refresh/start/list/get helpers
  - pricing, context-window, max-output, capability, modality lookups
  - cost estimation helper
  - tests using `httptest`
- `go-modelsdev v0.2.0` added to `go.mod`.

## Why this boundary is good

The design and first code slices establish the reusable substrate without
locking the daemon/API surface too early:

- the gateway shape is documented
- middleware already matches the successful MCP proxy pattern
- model metadata now has a sanctioned Tether wrapper
- the repo stays green

This is a good point to begin real implementation slices in the next session.

## Verification

Passed:

- `go test ./internal/llm ./internal/modelcatalog`
- `go test -count=1 ./...`

## Recommended next slices

1. `internal/llm/secrets`
   - define secret references
   - invoke `mux-apikey-helper`
   - return secret material only at runtime
   - tests with fake helper binary / temp script

2. `internal/llm/router`
   - define route policy types
   - implement simple explicit routing:
     provider pin, model pin, capability checks, budget-aware fallback
   - wire `internal/modelcatalog` into route validation

3. First provider adapter
   - prefer `internal/llm/openai` or `internal/llm/anthropic`
   - use official SDK
   - disable SDK retries
   - implement a minimal non-streaming chat path first

4. In-process AI service
   - one service method that accepts normalized `llm.Request`
   - route through middleware chain
   - call one provider adapter
   - return normalized `llm.Response`

5. Daemon HTTP surface
   - likely start with `POST /ai/chat`
   - keep it typed and Tether-native, not vendor passthrough

## Notes

- The AI gateway should remain separate from session runtimes at first.
  API-backed launches can later reuse the same adapters and secrets/model
  layers.
- The existing `mux-apikey-helper` seam in `internal/app/runtime_helpers.go`
  is the preferred starting point for secret resolution.
- The existing MCP middleware and `proxy_events` durability are the best
  local prior art for policy + observability.

## Beta follow-on threads

Updated: 2026-05-25

Core beta proxy functionality is now in place, including:

- configured Anthropic, OpenAI, and OpenAI-compatible providers
- typed daemon AI surfaces, CLI surfaces, and MCP surfaces
- route policy, route explain, usage, audit, durable budgets, and live budget alerts
- general durable/live event history surfaces

Remaining threads worth continuing after the streaming push:

1. General event-history filtering ergonomics
   - add timestamp-based filters to `GET /events`, `mux events history`, and `mux_events_history`
   - consider richer paging/export patterns for operator workflows

2. Operator summaries over raw rows
   - add higher-level event / AI audit summary surfaces so common triage does not require reading raw event rows

3. Setup and operator workflow docs
   - expand real-provider examples
   - document recommended HTTP / CLI / MCP usage patterns for the AI gateway

4. Provider expansion
   - additional provider adapters beyond Anthropic / OpenAI / OpenAI-compatible

5. MCP-native AI streaming
   - HTTP + CLI streaming is the current priority and best fit
   - true MCP live token streaming likely needs MCP resources or a different long-lived transport shape than the current tool request/response model
