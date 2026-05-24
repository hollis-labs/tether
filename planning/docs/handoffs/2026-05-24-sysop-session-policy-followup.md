# Handoff — Sysop session policy follow-up

Date: 2026-05-24

## What landed

- Daemon/API/client policy surface for logical agents:
  - `GET /logical-agents/{id}/policy`
  - `PATCH /logical-agents/{id}/policy`
- First real daemon-honored policy:
  - `checkpoint_policy=on_stop`
  - `app.Service.StopSession` now creates a checkpoint before stop when that
    policy is enabled.
- Sysop GUI slice:
  - session detail has a `Policy` action
  - policy dialog can read/save logical-agent checkpoint policy
  - only `manual` and `on_stop` are exposed
- Docs updated:
  - `docs/api/README.md`
  - `planning/docs/sysop-settings-roadmap.md`

## Files to start from

- Backend/shared surface
  - `internal/agent/policy.go`
  - `internal/store/logical_agents.go`
  - `internal/api/checkpoints.go`
  - `internal/client/client.go`
  - `internal/app/logical_agent_policy.go`
  - `internal/app/session_lifecycle.go`
  - `internal/daemon/server.go`
  - `cmd/mux/daemon.go`
- Sysop
  - `apps/sysop/cmd/tether_sysop/main.go`
  - `apps/sysop/frontend/src/api/client.ts`
  - `apps/sysop/frontend/src/components/session-detail-dialog.tsx`
  - `apps/sysop/frontend/src/pages/operations.tsx`

## Current behavior

- `checkpoint_policy=manual`
  - stop behaves as before
- `checkpoint_policy=on_stop`
  - stop checks the logical agent policy
  - if enabled, it writes a checkpoint with status:
    - configured `checkpoint_status`, or
    - fallback `auto-stop`
  - if checkpoint creation fails, stop returns the error and does not proceed
  - auto-stop checkpoints now carry a richer payload:
    - summary includes session/launch/provider/state/workspace context
    - key_decisions explains policy origin
    - next_recommendation points operators to logical-agent resume

- `GET /logical-agents` now includes compact policy summary:
  - `checkpoint_policy`
  - `checkpoint_status`

## Known limits

- Only `checkpoint_policy` is real today.
- `hot_cold_policy` is still storage-only.
- No backend-honored attach-mode or cleanup/retention policy exists yet.
- There is still no live provider hint capture in this path because
  `agentsessions.Manager` exposes only `SessionInfo`, not raw session handles
  or `CheckpointHints()`.

## Best next slice

1. Improve auto-stop checkpoint fidelity.
   - If there is a clean way to surface provider checkpoint hints through
     `agentsessions.Manager`, wire that into `CreatePolicyCheckpoint`.
   - Keep derived summary/status metadata honest; do not synthesize provider
     details the runtime cannot actually supply.
2. Only then consider broader policy expansion.
   - Do not expose attach/retention/hot-cold controls until the daemon can
     actually honor them.

## Verification used this session

```bash
go test ./internal/api ./internal/store ./internal/client ./internal/app
cd apps/sysop && go build ./cmd/tether_sysop
npm --prefix apps/sysop/frontend run typecheck
npm --prefix apps/sysop/frontend run build
cerberus resource reload tether-dev
```
