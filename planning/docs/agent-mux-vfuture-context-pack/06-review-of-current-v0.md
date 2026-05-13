# Review of Current v0

This section captures the observed shape of the current v0 from the attached repo.

## What is already good

- The project already separates config/catalog, launch, workspace, provider, store, session, and app service concerns.
- SQLite-backed durable session records already exist.
- Workspace materialization is present.
- PTY-backed provider launch works.
- Provider adapter registry is already the correct general direction.
- The current shape is viable as the seed of a durable runtime/control plane.

## What must change next

### 1. One-shot CLI runtime is not enough
The current implementation creates service state per command. Live ownership of running sessions is in-memory only, which means new invocations cannot truly attach to already-owned sessions.

### 2. Attach is snapshot-only
The current attach behavior reads from the log file and does not provide live attach, tail-follow, or input injection to a running PTY.

### 3. Stop only works for current process-owned sessions
If runtime ownership is not daemonized, stop and control operations are limited.

### 4. Provider env handling is too aggressive
The current Claude Code adapter replaces the environment instead of merging controlled overrides with inherited base environment.

### 5. Shared-state concurrency must become explicit
The current maps for running/killing need correct synchronization once there are multiple clients, API calls, or background lifecycle management.

## Action from this review

Do not rewrite the current v0 from scratch. Use it as the base, but refactor toward:

- daemon/service mode
- live attach
- input injection
- event bus
- checkpoint model
- broker model
- stable local API
