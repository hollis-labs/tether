# Tether AI Gateway Spec

Status: proposed
Date: 2026-05-24
Owner: Tether

## Summary

Tether will add a first-class AI gateway that sits beside the existing session,
daemon, MCP, messaging, and registry surfaces. The gateway gives operators one
local API endpoint for LLM access while Tether owns:

- provider and model routing
- secret resolution via external keychain helper
- policy and middleware
- request/usage auditability
- budget and safety enforcement
- future API-backed launches

This is not a thin reverse proxy. It is a policy-aware local control plane for
AI API traffic in the same way `mux mcp --proxy` is a policy-aware control
plane for MCP tools.

## Product framing

The AI gateway serves two user groups:

1. apps in the Hollis Labs portfolio that need a stable local AI surface
2. operators who want one endpoint with stronger controls than direct vendor
   API usage

The MVP story is:

- configure Anthropic, OpenAI, and OpenAI-compatible providers in Tether
- keep secrets in the system keychain, not in Tether state
- expose one local "AI" API
- route calls by policy instead of hardcoding provider/model choices in each
  client
- record usage and route decisions for audit and debugging

Post-MVP expands into embeddings, image/audio/video APIs, Azure, GitHub
Copilot, and enterprise governance needs.

## Goals

- One local AI API surface for portfolio apps and external callers.
- Provider/model routing driven by policy, not caller-specific conditionals.
- Runtime secret resolution through `mux-apikey-helper` or equivalent external
  helper. Tether stores references, never secret material.
- Reusable middleware for PII scrubbing, audit, rate/budget enforcement,
  request shaping, and response shaping.
- Durable observability: route decisions, usage, latency, errors, budget hits.
- Shared model metadata via `go-modelsdev` for pricing, context windows,
  modalities, and capabilities.
- A foundation that can also support true API-backed launches.

## Non-goals

- Do not ship every provider or every modality in the MVP.
- Do not start with a generic reverse proxy that forwards arbitrary vendor HTTP
  payloads without normalization.
- Do not persist API keys, OAuth tokens, or raw sensitive request bodies in
  Tether state.
- Do not force the existing session runtime contract to absorb all AI gateway
  concerns on day one.

## Existing Tether prior art

Tether already has several load-bearing pieces that should be reused:

- MCP proxy aggregation and routing:
  `internal/mcpadapter/{client_pool,proxy,proxy_adapter}.go`
- MCP proxy middleware:
  `internal/mcpadapter/middleware.go`
- MCP proxy observability and privacy stance:
  `docs/adr/0021-mcp-proxy-observability.md`
- durable proxy event persistence:
  `internal/store/proxy_events.go`
  `internal/api/proxy_events.go`
- provider runtime matrix and future `runtime_kind=api` support:
  `docs/adr/0029-api-provider-reframe.md`
  `docs/adr/0037-provider-runtime-kind-matrix.md`
- external secret-helper seam:
  `internal/app/runtime_helpers.go`

The AI gateway should follow the MCP proxy pattern closely: normalized request
surface, routing core, middleware chain, durable observability, and explicit
privacy defaults.

## Architecture

```text
caller app / agent / GUI / CLI
            |
            v
     Tether daemon AI surface
            |
            v
        route planner
            |
            v
      middleware chain
            |
            v
   provider adapter (Anthropic / OpenAI / OpenAI-compatible)
            |
            v
       official vendor SDK
```

This lives as a new subsystem under `internal/llm/` plus a small shared model
catalog wrapper.

## Package layout

Planned package layout:

- `internal/llm/`
  Normalized request/response types and middleware contract.
- `internal/llm/router/`
  Route planning and policy evaluation.
- `internal/llm/secrets/`
  Runtime secret resolution via helper/keychain reference.
- `internal/llm/observability/`
  Event/audit/usage emission helpers and DTOs.
- `internal/llm/anthropic/`
  Anthropic SDK wrapper.
- `internal/llm/openai/`
  OpenAI SDK wrapper.
- `internal/llm/openaicompat/`
  OpenAI-compatible wrapper with configurable base URL and auth shape.
- `internal/modelcatalog/`
  Tether wrapper over `go-modelsdev`.

Potential later packages:

- `internal/llm/embeddings/`
- `internal/llm/images/`
- `internal/llm/audio/`
- `internal/llm/copilot/`
- `internal/llm/azureopenai/`

## Why not just `httputil.ReverseProxy`

Go's standard library has `net/http/httputil.ReverseProxy`, but it is too low
level for Tether's real problem.

Tether needs semantic routing, model capability checks, policy enforcement,
secret indirection, and per-request audit. A byte-level HTTP reverse proxy can
be an implementation detail for a specific adapter later, but it should not be
the design center.

## Canonical request model

The gateway needs a normalized request model independent of any one vendor.
That model should carry:

- operation kind
  chat completion, embedding, image generation, transcription, etc.
- provider/model hints
- session/caller metadata
- mode and intent hints
- budget hints
  token, cost, latency
- content parts
  text/image/audio/file
- tool schema definitions
- routing metadata

The first landed slice only needs the common primitives for chat and future
multimodal expansion.

## Middleware model

Tether should use a middleware chain for AI requests just as it already does
for MCP tool calls.

Candidate interface:

```go
type Middleware interface {
    Handle(ctx context.Context, req Request, next Handler) (Response, error)
}
```

Initial middleware targets:

- `AuditMiddleware`
  emit durable request/response summary + route decision
- `PIIMiddleware`
  scrub or reject sensitive content according to policy
- `BudgetMiddleware`
  reject or downgrade routes that exceed token/cost limits
- `CapabilityMiddleware`
  reject routes that cannot satisfy modality/tool/caching requirements
- `PolicyMiddleware`
  enforce mode/intent/provider allowlists
- `RetryMiddleware`
  explicit, policy-owned retry; vendor SDK retries remain disabled

Important default:

- middleware must not persist raw sensitive content by default
- observability should record schema/shape/summary/usage, not full payloads

## Secrets model

Tether already documents that secrets should not be persisted in plans or the
registry and already has a `mux-apikey-helper` seam.

The AI gateway should formalize this:

- provider configuration stores a secret reference, not a key
- secret reference examples:
  `keychain://openai/personal`
  `keychain://anthropic/work`
  `helper://mux-apikey-helper/openai/default`
- runtime resolution happens just-in-time before the SDK call
- resolved secret material is never persisted to `state.db`

Implementation note:

- the first implementation should prefer the existing external helper seam over
  embedding direct OS keychain dependencies into Tether
- a direct `go-keyring` backend can be added later if it materially improves
  deployment and remains policy-compatible

## Model catalog

Tether should adopt `go-modelsdev` like Nanite and Torque.

Why:

- provider and model discovery
- pricing data
- context window data
- capability flags
- modality support
- feature gating and route validation

The wrapper should be Tether-owned and nil-safe so callers can degrade
gracefully on a cold cache. Torque's `internal/modelcatalog` is the closest
prior-art shape for this.

The model catalog should be treated as advisory metadata, not as the only
source of truth for provider support. Tether policies and provider adapters
still own final validation.

## Provider support

### MVP

- Anthropic
  official `anthropic-sdk-go`
- OpenAI
  official `openai-go`
- OpenAI-compatible
  base-URL-driven adapter for local/self-hosted or compatible services

### Post-MVP

- Azure OpenAI
- GitHub Copilot
- embeddings
- image generation
- audio input / transcription / TTS
- richer multimodal APIs

## Routing policy

Routing should be policy-driven and composable.

Inputs to route planning:

- explicit provider/model pin from caller
- mode
  chat, coding, retrieval, planning, classification, utility
- intent
  reasoning, extraction, summarize, tool-heavy, low-latency, low-cost
- required capabilities
  tools, reasoning, attachment/image input, temperature control
- budget
  token ceiling, output cap, cost ceiling, latency target
- request size / complexity
- privacy tier / compliance tier
- user or org defaults

Route planning should produce a `RouteDecision` with:

- chosen provider
- chosen model
- reasons
- policy version
- downgrade/fallback notes

That route decision should be part of observability.

## Tether surfaces

The AI gateway should become available over multiple Tether surfaces, but in
phases.

### Phase 1

- in-process service package
- daemon HTTP surface
  minimal typed endpoints for chat completion
- internal callers only

### Phase 2

- `mux ai ...` CLI
- MCP tools for introspection and controlled invocation
- Sysop visibility

### Phase 3

- ACP or other client-facing exposure if justified

## Proposed daemon HTTP surface

Initial typed endpoints:

- `POST /ai/chat`
- `GET /ai/providers`
- `GET /ai/models`
- `GET /ai/routes`
  optional route preview / explain
- `GET /ai/usage`
- `GET /ai/audit`

Potential later endpoints:

- `POST /ai/embeddings`
- `POST /ai/images`
- `POST /ai/audio/transcriptions`
- `POST /ai/audio/speech`

The daemon API should be typed and normalized. It should not start as a
vendor-specific request passthrough.

## Observability

Reuse the proxy-events pattern but with AI-specific records.

Suggested durable row shape:

- request id
- session id
- caller id
- operation kind
- provider
- model
- route policy/version
- latency
- success/failure
- refusal/block reason
- input tokens
- output tokens
- cache read/write tokens
- reasoning tokens when available
- estimated cost
- sanitized request summary
- sanitized response summary

Important privacy default:

- raw prompts and raw responses should not be durably stored by default
- opt-in verbose capture can exist later, but only as an explicit policy mode

## Relationship to API-backed launches

The AI gateway and API-backed launches are related but not identical.

- AI gateway:
  request/response service for direct LLM access
- API-backed launches:
  session runtime that uses an LLM API as its execution engine

The gateway should be built first as its own subsystem. Then API-backed
launches can consume the same vendor adapters, model catalog, secret
resolution, and middleware/policy components.

This keeps the session runtime contract clean while still enabling
`runtime_kind=api` launches later.

## Configuration direction

Tether should gain a catalog/config surface for AI providers and routing
policy. Proposed shape:

```yaml
ai:
  providers:
    - id: anthropic-work
      type: anthropic
      secret_ref: keychain://anthropic/work
      enabled: true
    - id: openai-personal
      type: openai
      secret_ref: keychain://openai/personal
      enabled: true
    - id: llama-local
      type: openai-compatible
      base_url: http://127.0.0.1:11434/v1
      secret_ref: ""
      enabled: true
  routing:
    default_mode: chat
    default_provider_order: [anthropic-work, openai-personal, llama-local]
    policies:
      - match:
          mode: utility
        route:
          provider: openai-personal
          model: gpt-4o-mini
      - match:
          requires_reasoning: true
        route:
          provider: anthropic-work
          model: claude-sonnet-4-5
```

This exact schema should be treated as direction, not yet locked contract.

## MVP slices

Recommended slices:

1. design + package scaffolding
2. `go-modelsdev` wrapper
3. normalized request/response + middleware contract
4. secret reference + helper invocation package
5. Anthropic adapter
6. OpenAI adapter
7. OpenAI-compatible adapter
8. route planner + policy model
9. in-process AI service
10. daemon HTTP endpoints
11. durable usage/audit store
12. API-backed runtime built on the same adapters

## First implementation constraints

- use official SDKs
- disable SDK retries
- prefer streaming-friendly normalized types
- do not hand-roll HTTP clients when an official SDK exists
- do not persist secret material
- do not persist raw sensitive prompts/responses by default
- keep the first route planner simple and explicit

## Open questions

- Should OpenAI-compatible providers be treated as one generic adapter or a
  family of named provider kinds?
- Should route policies live only in catalog YAML or also be mutable through
  Sysop/UI APIs?
- Should Tether expose an OpenAI-compatible facade as one of its public
  gateway protocols, or keep the first API purely Tether-native?
- Should AI observability live in a dedicated table from the start, or begin
  with event-bus publication plus a durable side table similar to
  `proxy_events`?
- When API-backed launches arrive, should they call the in-process AI service
  or go directly to the shared vendor adapters?

## Immediate next steps

- land `internal/llm` package scaffolding and middleware contract
- land `internal/modelcatalog` backed by `go-modelsdev`
- define the secret reference abstraction and helper invocation shape
- add the first provider adapter using the official SDK
