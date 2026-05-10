# ADR 0029 — Reframe the Parked API-Provider Sprint

**Status:** Accepted
**Date:** 2026-05-09
**Supersedes:** the original v005-04 "C1 API provider via go-providers" sprint framing
**Superseded by:** —

## Context

The parked C1 plan assumed mux would:

- construct a `provider.Provider` from `go-providers`
- wrap it into a `go-agent-sessions.Session`
- add API-backed launches beside the CLI-backed launches

That plan is no longer structurally valid.

Between `go-providers v0.10.0` and `v0.13.0`:

- HTTP provider constructors such as `provider.NewAnthropic` and `provider.NewOpenAI` were deleted.
- provider interfaces and rate-budget primitives moved into `go-llm-contracts`.
- transport-neutral request/stream/message types moved into `go-llm-types`.

Sibling apps already shipped the new shape:

- Nanite: `internal/llm/{anthropic,openai}/`
- Vanta-Conduit: `internal/llm/{anthropic,openai}/`
- Clockwork-Manifold: plugin/executor-specific SDK wrappers

## Decision

Mux will not implement an API provider in this sprint.

When the need returns, mux will build per-vendor wrappers in `internal/llm/<vendor>/` that implement `go-llm-contracts`, following the portfolio migration guide and the sibling-app reference implementations.

Locked defaults for that future sprint:

- Anthropic SDK: `github.com/anthropics/anthropic-sdk-go`
- OpenAI SDK: `github.com/openai/openai-go`
- disable SDK retries with `option.WithMaxRetries(0)`
- preserve mux's session-shaped consumer interface at the boundary
- treat streaming as the likely default for mux because it already has CLI stream surfaces
- skip rate-budget middleware unless a concrete mux seam exists that justifies it

## Consequences

- The existing planning-pack sprint is renamed and repurposed to the lib-tier bump.
- No `go-llm-contracts`, `go-llm-types`, or `go-embed-contracts` dependency is pulled into mux today.
- Future API-provider work must start from the portfolio guide, not from pre-reshape assumptions in old mux notes.
