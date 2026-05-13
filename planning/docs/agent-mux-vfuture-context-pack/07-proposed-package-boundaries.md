# Proposed Package Boundaries

This is a target package map, not a rigid prescription.

## cmd/
Thin entrypoints only.

Examples:

- cmd/mux
- cmd/muxd (optional dedicated daemon entrypoint)

## internal/catalog
Loads and validates catalog/config references consumed by Agent Mux.
This is runtime-facing resolution, not full Agent Ops ownership.

## internal/launch
Turns launch inputs into a resolved runtime plan.

## internal/workspace
Creates/manages per-session workspace materialization.

## internal/provider
Provider runtime contract and implementations.

Subpackages might include:

- internal/provider/cli/claudecode
- internal/provider/api/openai
- internal/provider/api/anthropic

## internal/runtime
Long-lived daemon/runtime ownership of active sessions and lifecycle transitions.

Potential responsibilities:

- registry of active handles
- lifecycle transitions
- attach manager
- input injection
- stop/restart
- checkpoint trigger coordination

## internal/session
Runtime session primitives: PTY helpers, stream handling, runtime state models.

## internal/agent
LogicalAgent models and policy references.

## internal/checkpoint
Checkpoint/handoff creation, storage, and resume helpers.

## internal/broker
Mailbox/envelope handling, correlation, routing.

## internal/store
SQLite access and persistence layer.

## internal/events
Event model and pub/sub or subscription helpers.

## internal/api
Local API handlers/transport adapters.

## internal/app
Composition root only.
Avoid allowing this package to become a god-package.

## pkg/ (optional)
Only export generally reusable packages when there is a clear external consumer need. Do not prematurely export internals.
