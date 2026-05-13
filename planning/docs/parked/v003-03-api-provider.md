# Sprint v003-03 — First API-Backed Provider

Epic: [[Parked] Integration Foundation](./v0.0.3-integration-foundation.md)

**Epic:** [[Parked] Integration Foundation](./v0.0.3-integration-foundation.md)
**Goal:** Implement the first concrete API-backed provider (not a stub). Anthropic or OpenAI SDK. Reuse the streaming abstraction introduced in Sprint v002-04. Prove that a non-PTY runtime fits the same session model (launch / attach / input / stop / events).
**Exit criteria:**
- [ ] A concrete `internal/provider/api/<vendor>/` package implements the `Runtime` and `Session` interfaces from Sprint v002-04.
- [ ] `mux launch --launch <api-launch-id>` creates a session that streams SDK responses through the attach stream.
- [ ] Input (user messages) delivered via `POST /sessions/{id}/input` reaches the SDK conversation.
- [ ] Stop cleanly cancels in-flight SDK calls.
- [ ] Attach stream renders SDK streaming chunks as they arrive.

## Context

Context-pack §03 v0.0.3 includes "first API-backed provider implementation." Context-pack §04 use case 8 ("API-backed non-CLI execution") is the scenario. Context-pack §08 says CLI and API runtimes should both fit the same higher-level session contract. Sprint v002-04 introduced the `Runtime` interface and the `api-stub` to prove the shape; this sprint replaces the stub with a real vendor integration.

Current state:
- `internal/provider/` contract supports non-PTY runtimes (after v002-04).
- `api-stub` demonstrates the shape.
- No real SDK is wired yet.

## Tasks

### T-v003-s03-01: Pick vendor + scaffold package

**kind:** decision + agent
**priority:** 1
**manual:** true
**tags:** [decision, provider, api]

#### Problem

Anthropic or OpenAI? Both have streaming Go SDKs. Anthropic is closer to the existing `claude-code` CLI (same vendor, same model family) so context-pack alignment is easier; OpenAI has broader tool-calling patterns.

#### Fix direction

- Recommend Anthropic first for vendor continuity with `claude-code`.
- Create `internal/provider/api/anthropic/`.
- Add YAML catalog schema support for API-provider config: `id`, `type: api`, `sdk: anthropic`, `model`, `api_key_env` (name of env var to read), `system_prompt_source` (inherit from launch boot prompt).
- Capture the decision as ADR.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/provider/api/anthropic/` (new)
- `/Users/chrispian/Projects-apps/agent-mux/internal/config/model.go` — extend Provider struct if needed
- `/Users/chrispian/Projects-apps/agent-mux/examples/catalog/providers/anthropic-claude.yaml` (new)
- `/Users/chrispian/Projects-apps/agent-mux/docs/adr/0005-first-api-provider.md` (new)

#### Acceptance criteria

- [ ] Directory exists with package skeleton.
- [ ] YAML catalog entry parses.
- [ ] ADR committed.

#### Test plan

- Build green. YAML loads. No SDK calls yet.

#### Scope fences

- Do not start SDK calls in this task — scaffold only. The real implementation is T-v003-s03-02.
- Do not lock in OpenAI later; vendor-agnostic abstractions are valuable. But don't build them speculatively either.

#### Relationship

Depends on: T-v002-s04-02.
Blocks: T-v003-s03-02.

#### Origin

Context-pack [04-use-cases.md](../agent-mux-vfuture-context-pack/04-use-cases.md) use case 8, [07-proposed-package-boundaries.md](../agent-mux-vfuture-context-pack/07-proposed-package-boundaries.md).

---

### T-v003-s03-02: Implement streaming SDK wrapper

**kind:** agent
**priority:** 1
**manual:** true
**tags:** [feature, provider, api, streaming]

#### Problem

Need a concrete `Runtime`/`Session` implementation that:
- Opens an Anthropic Messages stream on `Start`.
- Appends user messages on `SendInput`.
- Pipes streaming chunks to the attach broker.
- Cancels the stream on `Stop`.

#### Fix direction

- Use the official Anthropic Go SDK (`github.com/anthropics/anthropic-sdk-go` or equivalent).
- `Start`: open the SDK client, build initial Messages request from the boot prompt, begin streaming. First response streams chunks into the attach broker.
- `SendInput`: append a user message; start a new streaming turn. Serialize turns (one at a time).
- `Stop`: cancel the context on the active stream; mark session completed.
- Health: last-activity timestamp; expose via `Session.Health()`.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/provider/api/anthropic/runtime.go`
- `/Users/chrispian/Projects-apps/agent-mux/internal/provider/api/anthropic/session.go`

#### Acceptance criteria

- [ ] `mux launch --launch anthropic-demo` streams a response to the attach stream.
- [ ] `mux sessions input <id> "follow-up question"` yields a second response.
- [ ] `mux sessions stop <id>` cleanly cancels.
- [ ] The session's state transitions match what the CLI provider does (`running` while streaming, `completed` when done, `failed` on error).

#### Test plan

- Unit: mock SDK client; assert correct request/response handling, cancellation propagation.
- Integration (gated by `ANTHROPIC_API_KEY`): actual SDK call, end-to-end.

#### Scope fences

- Do not implement tool use / function calling in this sprint — text streaming only. Tool use is v0.1+.
- Do not add multi-vendor abstraction beyond what's already in the Runtime interface.
- Do not hide API costs from the user — log token usage to the session events.

#### Relationship

Depends on: T-v003-s03-01.
Blocks: T-v003-s03-03.

#### Origin

Context-pack [04-use-cases.md](../agent-mux-vfuture-context-pack/04-use-cases.md) use case 8.

---

### T-v003-s03-03: Delete (or quarantine) `api-stub`

**kind:** agent
**priority:** 3
**manual:** true
**tags:** [cleanup, provider]

#### Problem

The `api-stub` from Sprint v002-04 was explicitly marked as throwaway. Once a real API provider exists, the stub is noise.

#### Fix direction

- Delete `internal/provider/api/stub/`.
- Remove the `api-stub.yaml` catalog entries.
- Keep the pattern (anyone can add a new stub later) but don't ship a demo stub in the catalog.

#### Files

- `/Users/chrispian/Projects-apps/agent-mux/internal/provider/api/stub/` (delete)
- `/Users/chrispian/Projects-apps/agent-mux/examples/catalog/providers/api-stub.yaml` (delete)
- `/Users/chrispian/Projects-apps/agent-mux/examples/catalog/launches/api-stub-launch.yaml` (delete)

#### Acceptance criteria

- [ ] Files gone; build green; tests green.
- [ ] No orphaned imports.

#### Test plan

- Build + test.

#### Scope fences

- Do not retain the stub as a test helper if a clean test double can be built from the `Runtime` interface instead.

#### Relationship

Depends on: T-v003-s03-02.

#### Origin

Sprint v002-04 readiness note ("document this"); this task closes that loop.

## Review / readiness notes

- **API key handling:** recommend reading from env var (per catalog's `api_key_env` field) and never logging the value. Document in `internal/provider/api/anthropic/README.md`.
- **Cost observability:** token counts should flow to events so Clockwork can enforce budgets later. Expose as an event payload field.
- **Streaming back-pressure:** if the attach broker is slow (no subscribers / full buffer), what happens to the SDK stream? For v0.0.3, keep it simple: continue streaming and drop on a full buffer (matching the attach broker's existing policy).
- **Cancellation timing:** SDK calls may take several seconds to actually cancel. Document the expected lag in the ADR.
