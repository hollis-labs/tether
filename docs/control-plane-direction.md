# Tether Platform Direction

**Status:** Directional architecture draft

**Date:** 2026-08-22
**Scope:** Desired product boundaries and architecture; not an implementation plan

## Purpose

This document describes Tether's intended place in the Hollis Labs portfolio.
It evaluates the existing session runtime, LLM gateway, MCP gateway, messaging,
federation directory, configuration, credential, plugin, and observability
surfaces against the portfolio's engineering boundaries.

It does not define phases, estimates, task breakdowns, migration steps, or a
final settings-precedence model. Existing ADRs remain implementation and
historical truth until explicitly superseded. This document provides direction
for a later architecture and planning session.

## Portfolio axiom

> Hollis tools own execution and operational state, but not the business
> definitions or business data they operate on.

For Tether, this means:

- Callers own prompts, agent definitions, project configuration, task intent,
  tool semantics, and workflow policy.
- LLM providers own model behavior and provider-native state.
- MCP servers own tool implementations and the data behind them.
- Senders and recipients own the meaning of message content.
- Substrates own their operational configuration and resource state.
- Credential authorities own secret material and its lifecycle.
- Tether owns the operational state required to route, execute, deliver,
  enforce, observe, and recover the services it provides.

Tether may persist message envelopes, session events, route decisions, usage,
and delivery state because those are part of its execution contract. That
custody does not make Tether the semantic owner of caller content.

## Product definition

Tether is an independently useful, local-first communication and gateway
platform with an optional agent-session substrate.

Its major capabilities are peers within one product:

1. Agent-session runtime for sessions explicitly launched through Tether.
2. LLM gateway for normalized provider access, routing, policy, and usage.
3. MCP gateway for discovery, proxying, policy, and observability.
4. Durable messaging and optional federation.
5. Public-identity directory for cross-substrate discovery.
6. Shared event, audit, and operational-observability surfaces.

Tether is not the mandatory runtime through which every Hollis agent or tool
must pass. Nanite, Torque, and other applications may own their own runtimes
and use only the Tether capabilities that improve their product.

## Architecture sketch

```text
                           callers
       +----------------------+-----------------------+
       |                      |                       |
       v                      v                       v
    Nanite                  Torque                other apps
       |                      |                       |
       +----------------------+-----------------------+
                              |
                     optional composition
                              |
                 +------------v-------------+
                 |          Tether          |
                 |                          |
                 |  session substrate       |
                 |  LLM gateway             |
                 |  MCP gateway             |
                 |  messaging               |
                 |  federation directory    |
                 |  events and audit        |
                 +--+--------+--------+-----+
                    |        |        |
                    v        v        v
                agents     models    MCP servers
                    |        |        |
                    +--------+--------+
                             |
                 external systems retain truth

Credential authorities ---- references / runtime grants ----> gateway boundary

Cerberus ---- optional workspace/resource handles ----> Tether session substrate
```

Each subsystem has its own stable contract. They compose through explicit
references, caller identity, policy, and events rather than through a single
universal "agent platform" abstraction.

## Independence and composition

Every portfolio application remains independently useful:

- Nanite owns and launches Nanite agents through its Agent Host without
  requiring Tether.
- Torque owns job, scheduling, and workflow execution and may choose its own
  execution substrate.
- Cerberus owns resource and workspace control without requiring Tether as its
  control path.
- Tether can launch and manage its own sessions without requiring Nanite or
  Torque.

Composition is optional and additive:

- An app may use only Tether's LLM gateway.
- An app may use only its MCP gateway.
- An app may participate only in messaging or directory discovery.
- An app may delegate a session to Tether as one provider path.
- Tether may acquire a Cerberus workspace while retaining its own session
  semantics.

Overlap in capability is acceptable when the ownership and use cases differ.
Shared mechanics should move into libraries or host contracts only after real
cross-app pressure exists; applications should not be forced behind another
application merely to eliminate surface overlap.

## Responsibility boundaries

| Concern | Authoritative owner | Tether responsibility |
|---|---|---|
| Caller intent and prompts | Calling application | Transport and enforce declared request policy |
| Caller-provided agent definition | Calling application | Validate and materialize for one Tether launch; do not persist by default |
| Tether internal agents | Tether | Own definitions and lifecycle for Tether product features |
| Tether-launched session | Tether | Lifecycle, execution, attach, recovery, events |
| Nanite or Torque session | Nanite or Torque | No implicit ownership; interact only through an explicit integration |
| LLM provider configuration values | Operator or calling scope | Resolve effective registration and policy |
| LLM request content | Caller | Route, execute, and observe under policy |
| MCP server configuration | Operator, project, or caller | Resolve registration, connect, proxy, and enforce grants |
| Tool semantics and backing data | Upstream MCP server | Discovery, routing, policy, and operational observation |
| Message meaning | Sender and recipient | Durable envelope delivery and delivery-state authority |
| Public identity | Owning substrate | Thin directory projection and lookup |
| Operational substrate data | Owning application | Hold references/callbacks; never mirror full payloads by default |
| Secret material | External credential authority | Carry references and resolve or inject only at execution time |
| Workspace lifecycle | Cerberus and workspace provider | Consume a workspace handle for Tether-owned sessions when configured |

## Agent-session substrate

Tether's session capability remains a real product capability, not merely a
gateway implementation detail. For sessions launched through Tether, it owns:

- Session identity and lifecycle.
- Provider/runtime binding.
- Process, stdio, streaming, attach, and cancellation mechanics.
- Boot-directory and execution-environment materialization.
- Tether-local workspace binding and execution root.
- Checkpoint, resume, and event history.
- Sandboxing and runtime capability reporting.
- Multi-client attachment and session-level observability.

It does not own:

- Task, sprint, job, workflow, review, or release semantics.
- The business meaning of an agent persona.
- Caller-owned project or role definitions.
- Nanite's agent lifecycle merely because Nanite also uses LLMs or MCP.

The existing two-tier agent model remains directionally sound:

- Tether-owned internal agents may be registered and persisted as product
  configuration.
- Caller-provided agents are launch inputs. Tether validates and materializes
  them for the requested session but does not silently adopt them into its
  catalog.

The statement "every agent session goes through Tether" is a Tether-local
product description, not a portfolio invariant.

## LLM gateway

The LLM gateway is a normalized, policy-aware execution surface rather than a
byte-for-byte vendor proxy.

Tether owns:

- Normalized request and response contracts.
- Provider and model capability discovery.
- Provider-registration resolution.
- Route evaluation and explainable route decisions.
- Policy and middleware enforcement.
- Usage, budgets, latency, errors, and sanitized audit records.
- Provider adapter conformance.

The caller owns:

- Prompt and attachment content.
- Semantic intent.
- Required capabilities and request-local constraints.
- Explicit provider/model preference where policy permits it.
- Business interpretation of the response.

The operator owns hard ceilings and installed provider registrations. Tether
may choose defaults and fallbacks only through an explicit, explainable
settings model. It must not silently override caller intent or make routing
policy indistinguishable from provider mechanics.

Raw prompts and responses are not durable observability by default. Audit
records should capture request shape, caller, route decision, provider/model,
usage, latency, policy outcomes, errors, and sanitized summaries. Full payload
capture is an explicit policy mode with clear retention and access controls.

API-backed agent sessions may reuse the gateway's adapters, middleware,
credential resolution, and model metadata. That reuse does not collapse the
request/response gateway and session runtime into one domain model.

## MCP gateway

The MCP gateway provides one controlled access layer over independently owned
tool servers.

Tether owns:

- Server registration and transport lifecycle for configured upstreams.
- Tool discovery, namespace resolution, and progressive disclosure.
- Capability, scope, allowlist, and policy enforcement.
- Invocation routing, cancellation, and normalized failures.
- Upstream health and compatibility observations.
- Sanitized tool-call audit and usage events.

Upstream servers own:

- Tool meaning and implementation.
- Tool-specific authorization beyond the gateway grant.
- Backing data and provider-native side effects.
- Provider-specific schemas and richer capabilities.

Tether should preserve upstream schemas and provider-specific capability
metadata rather than flatten tools into a lowest common denominator. A tool
gateway is an enforcement and observability boundary, not a reason to duplicate
tool implementations or cache their business payloads.

General MCP registration, direct tool invocation, and dynamically discovered
tools must all pass through the same effective grant model. A server-level
allowlist is useful but is not the final per-call authorization boundary.

## Messaging

Tether is authoritative for the operational messaging contract:

- Envelope identity and addressing.
- Durable storage and ordering.
- Delivery, read, consume, archive, cancel, and retry state.
- Subscription and federation routing.
- Delivery diagnostics and audit.

The sender and recipient remain authoritative for what a message means and how
it affects their work. Tether does not infer task completion, workflow state,
urgency policy, or command authority from message prose.

Message content is necessarily held in custody to provide durable delivery.
Retention, encryption, redaction, and deletion policy must therefore be
explicit. Operational metadata may remain after content retention expires when
needed for audit, provided the two are modeled separately.

Federation routes by address authority. It should not fork envelope schemas or
copy another application's messaging business logic into Tether.

## Federation directory

The current two-store directory model is the reference implementation of the
portfolio axiom:

```text
Owning substrate
  authoritative operational configuration and full content

Tether directory
  public identity, discovery fields, health projection, links, callback
```

Tether owns directory identity and search. Each substrate owns its operational
payload. Callback synchronization extracts only the declared public projection;
raw source payloads are not cached.

Secret relationships are opaque references. The directory never contains
secret material. Kind-specific metadata remains identity-oriented and must not
become a shadow operational database for Cerberus, Nanite, Torque, or other
substrates.

## Configuration and settings ownership

Tether should own configuration schemas, validation, resolution mechanics, and
effective-configuration explanations. It should not automatically own every
value that passes through those mechanisms.

Settings may originate from several authorities:

- Built-in compatibility defaults.
- Operator-wide policy and installed registrations.
- User configuration.
- Project configuration.
- Agent, launch, or boot profile.
- Calling application.
- Per-request or per-session overrides.

The final precedence and mutability rules require a dedicated architecture
decision. Whatever model is selected must satisfy these constraints:

1. Hard policy ceilings are distinguishable from defaults.
2. Caller intent is preserved unless a named policy denies or constrains it.
3. Denial explains which authority and rule produced it.
4. Caller-provided content is not persisted unless registration is explicit.
5. Effective values retain source provenance.
6. Secrets remain references at every persisted layer.
7. GUI, CLI, API, MCP, and library surfaces resolve through one settings core.
8. Applications can use Tether without adopting the full Tether catalog.
9. Standalone Tether remains useful with no portfolio configuration.

Provider registrations, MCP server registrations, messaging peers, and session
providers should use the same broad registration principles without being
forced into one undifferentiated schema.

## Credentials and secrets

Tether may provide credential plumbing but should not become the authority for
secret data.

Persisted configuration contains opaque references such as:

```text
keychain://openai/work
op://vault/item/field
helper://credential-helper/provider/account
```

The preferred execution modes are:

1. Delegate authentication to an official CLI or external provider session.
2. Ask an external helper to inject a credential into one child process.
3. Resolve a short-lived access grant scoped to audience and purpose.
4. Resolve plaintext into memory only for the duration of an SDK request when
   no delegated delivery mechanism exists.

Tether's main daemon stores references, never credential material. A helper
that writes to or deletes from the OS keychain is credential-authority tooling,
not a Tether state store; that distinction should remain explicit in naming,
packaging, and permissions.

Secrets must not enter launch plans, registry payloads, logs, route
explanations, MCP traces, message metadata, session events, or durable AI audit
records. Environment injection remains one delivery mechanism, not the
universal secret model.

## Extension model

Tether should use the Hollis Labs plugin SDK for common plugin mechanics while
retaining host-specific contracts for each subsystem.

The shared SDK may own:

- Manifest and compatibility negotiation.
- Process lifecycle and transport.
- Packaging, signing, catalog, and trust.
- Configuration schemas and secret-reference declarations.
- Capability declaration, grants, and preflight validation.
- Atomic registration and rollback.
- Health, diagnostics, and conformance harness infrastructure.

Tether-specific extension contracts may include:

- Agent/session runtime adapter.
- LLM provider adapter.
- MCP transport, server, middleware, or discovery provider.
- Messaging transport or federation peer adapter.
- Directory callback resolver or profile projector.
- Credential resolver or injector.

These should not be forced through one universal connector interface. The SDK
provides shared mechanics; each host contract preserves its own semantics.

Implementations may be:

1. Built-in Go adapters around official SDKs.
2. Adapters around official CLIs.
3. Subprocess plugins.
4. Custom protocol implementations when no supported tool exists.

Prefer official SDKs and CLIs when they provide stable contracts,
authentication, compatibility handling, and structured output. Custom clients
remain appropriate where no usable supported surface exists.

## Observability and privacy

Tether's cross-cutting value is a single operational view across its own
subsystems. Observability should normalize common envelope fields while
preserving raw provider events where diagnostics require them.

Common event provenance includes:

```text
event and correlation identity
caller and session identity
subsystem and operation
provider/server/peer identity
policy and route decision
capability grant
timing, usage, and outcome
sanitized error and diagnostic references
```

Event streams are operational data, not a general warehouse for prompts, tool
results, messages, or external payloads. Each subsystem declares its content
retention and redaction policy. The absence of a policy means sensitive raw
content is not persisted.

## Relationship to Cerberus workspaces

Tether does not own workspace infrastructure. If a Tether-launched session uses
a Coder workspace, Tether acts as one Cerberus client:

```text
Tether session create
       |
       v
Cerberus workspace acquire
       |
       v
WorkspaceHandle + WorkspaceLease
       |
       v
Tether binds its session runtime to the provided execution target
```

Tether owns the session-to-workspace binding and its session lifecycle.
Cerberus owns the workspace resource and lease. Coder owns provider-native
workspace infrastructure. The workspace never becomes part of Tether's
catalog merely because a session uses it.

This is optional composition. Local Tether workspace modes continue to operate
without Cerberus.

## Current-model strengths

Several existing Tether decisions already align strongly with this direction:

- The daemon-shaped session substrate has an explicit boundary from task,
  sprint, workflow, review, and release semantics.
- Caller-provided agent content is materialized for a launch without being
  persisted into Tether's catalog.
- The AI gateway stores secret references and resolves them just in time.
- The AI and MCP gateways use normalized contracts, middleware, policy, and
  durable operational observations.
- Messaging uses typed envelopes and authority-based federation.
- The directory stores public identity only and rejects raw operational payload
  caching.
- CLI, API, MCP, ACP, GUI, and Go-client surfaces converge on daemon/service
  contracts rather than owning separate business logic.

## Current-model tensions to revisit

- Product language that implies every portfolio agent session must use Tether
  conflicts with independent application ownership.
- Agent launching, AI gateway, MCP gateway, messaging, and directory are peer
  capabilities but can appear as one monolithic agent platform without a clear
  subsystem map.
- Settings ownership and precedence across operator, project, caller, launch,
  and request scopes are not yet one coherent contract.
- MCP server allowlists and scopes do not yet express full contextual per-call
  authorization.
- Some configuration loaders expand secrets into runtime config earlier than
  the narrowest possible execution boundary.
- The keychain helper combines resolve and credential-management commands; its
  external-authority role should remain clear.
- Session workspace modes currently model local filesystem placement, not an
  external provisioned workspace handle.
- Shared launch mechanics and app-specific agent semantics can drift if library
  extraction is driven by code similarity rather than proven shared contracts.
- Raw content retention differs by subsystem and needs explicit policy rather
  than incidental storage behavior.

These are architectural inputs, not a task list.

## Questions for the next architecture session

1. What is the exact authority and precedence model for operator, user,
   project, caller, launch, session, and request settings?
2. Which settings are hard policy ceilings, which are defaults, and which are
   caller-owned intent?
3. How does effective-configuration explanation work consistently across LLM,
   MCP, messaging, sessions, and federation?
4. What caller identity and grant model spans HTTP, MCP, ACP, CLI, GUI, and Go
   clients without assuming one application owns every call?
5. What is the contextual per-call authorization model for MCP tools and other
   gateway operations?
6. Which credential delivery mechanisms are supported for long-lived sessions,
   single LLM calls, MCP subprocesses, and federated peers?
7. What content-retention, encryption, and redaction policies apply separately
   to messages, prompts, responses, tool calls, and session output?
8. Which plugin SDK primitives are truly shared, and what Tether-specific
   adapter contracts sit above them?
9. How should Tether consume a Cerberus `WorkspaceHandle` without teaching the
   Tether session domain about Coder?
10. Which agent-launch mechanics should remain Tether-specific, which belong in
    shared libraries, and which overlaps with Nanite or Torque are intentional?
