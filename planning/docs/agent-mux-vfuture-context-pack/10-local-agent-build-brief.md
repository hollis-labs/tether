# Local Agent Build Brief

You are working on Agent Mux, a local-first Go application that is evolving from a launcher into a runtime/session control plane for agentic systems.

## Your mission

Produce the next maintainable foundation for Agent Mux with clean boundaries and OSS-friendly quality.

## Priority outcomes

1. Preserve what is already good in v0.
2. Introduce a durable local daemon/runtime model.
3. Add true attach/detach semantics.
4. Separate LogicalAgent from RuntimeSession.
5. Introduce a basic checkpoint/handoff path.
6. Introduce a basic broker/mailbox path.
7. Preserve provider-agnostic architecture with both CLI and API runtime support in mind.
8. Keep package boundaries clean and avoid god objects.
9. Reuse internal/company packages where appropriate before building new ones.
10. Keep the architecture compatible with Nanite, Clockwork, Agent Ops, Vanta, and Cerberus.

## How to work

- Review the current Agent Mux repo first.
- Review Nanite and Clockwork for patterns worth borrowing.
- Review internal reusable Go packages before creating new abstractions.
- Keep interfaces small and local.
- Prefer composition over huge service structs.
- Use migrations and clear schema evolution.
- Add tests where the behavior is easy and important to lock down.
- Leave concise architecture comments where future contributors will need them.
- If something should be deferred, defer it explicitly rather than smearing concerns into the wrong package.

## Anti-goals

Do not:

- build a GUI first
- couple the runtime to one vendor CLI
- turn Agent Mux into Clockwork
- turn Agent Mux into Agent Ops
- put memory logic directly inside Agent Mux that belongs in Vanta
- create a giant monolithic package named `app` that owns everything permanently
