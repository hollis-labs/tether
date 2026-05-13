You are working on Agent Mux, a local-first Go runtime/session control plane for agentic systems.

Read every file in this context pack before making changes.

Then inspect the current Agent Mux repository and orient yourself to:

- current package boundaries
- provider launch flow
- workspace materialization
- session runtime
- SQLite persistence
- current limitations around daemon ownership, attach/detach, and provider abstraction

Important context:

- Agent Mux is the execution substrate, not the planner and not the chat UI.
- Nanite is the interactive UX.
- Clockwork is the project/task/orchestration layer.
- Agent Ops will own asset/catalog/install concerns.
- Vanta is the memory primitive and may be embedded or service-backed.
- Cerberus is the service/process manager surface.

Your goal is to build the next maintainable foundation for Agent Mux, not to overbuild the full future at once.

Priorities:

1. Preserve the good shape of the current v0.
2. Introduce durable daemon/runtime ownership.
3. Add real attach/detach and input injection.
4. Separate LogicalAgent from RuntimeSession.
5. Add basic checkpoint/handoff primitives.
6. Add basic broker/mailbox primitives.
7. Keep the provider model compatible with both CLI and API runtimes.
8. Reuse existing internal packages where appropriate.
9. Follow strong Go hygiene: gopls, gofmt, goimports, golangci-lint, staticcheck, govulncheck, tests, clean package boundaries.
10. Avoid god objects and avoid conflating runtime, orchestration, UI, memory, and asset installation responsibilities.

Before building any new package or abstraction:

- inspect existing internal/company packages
- inspect Nanite and Clockwork for reusable patterns
- check whether the community already has a suitable package
- only build custom when it clearly improves fit, clarity, or control

Deliver work in coherent, reviewable increments with clear rationale.
