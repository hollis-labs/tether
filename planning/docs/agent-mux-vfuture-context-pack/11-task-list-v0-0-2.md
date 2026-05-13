# Suggested Task List for v0.0.2

## Milestone 1 — daemon/runtime mode
- create long-lived runtime service entrypoint
- move active session ownership into daemon lifecycle
- ensure sessions can be listed and queried by later client invocations
- add proper synchronization for active state

## Milestone 2 — live attach and input
- implement live attach stream instead of snapshot-only log copy
- implement send-input to running interactive sessions
- preserve log persistence and replay/snapshot support

## Milestone 3 — storage model evolution
- add LogicalAgent table/model
- evolve RuntimeSession table/model as needed
- add Checkpoint table/model
- add BrokerEnvelope table/model
- add migrations

## Milestone 4 — provider contract evolution
- refactor provider contract to support both CLI and API modes
- keep current Claude Code path working
- add stub or initial API provider implementation
- merge environment handling safely

## Milestone 5 — local API
- add minimal local API transport
- expose session lifecycle operations
- expose attach/input endpoints
- expose basic checkpoint and broker endpoints

## Milestone 6 — eventing
- add internal event bus or runtime event stream
- publish session lifecycle events
- publish broker events
- make it subscribable for future Nanite/Clockwork clients

## Milestone 7 — documentation and repo quality
- document architecture decisions
- add standard Go tool targets
- add CI-friendly commands
- add development setup notes
