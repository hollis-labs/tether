# ADR 0025: Provider Compliance Suite — Shared Behavioral Contract and Capability Matrix

**Status:** Accepted — 2026-04-23
**Context:** v0.0.4 Phase 4 — provider behavior testability
**Deciders:** agent-mux v0.0.4 execution session; task CW-20260423-0024
**Extends:** ADR 0006 (provider contract), ADR 0022 (Mux as optional provider substrate)

---

## Context

Agent Mux supports five provider adapters today (claude-code, claude-stream, opencode,
goprovider, api-stub) and will gain more. Each adapter satisfies the same
`provider.Runtime` + `provider.Session` interfaces, but their behavioral specifics differ:
PTY vs turn-based input, PID-bearing vs PID-less health, checkpoint-aware vs not.

Consumers (Clockwork, Nanite) need to:

1. Know which session lifecycle behaviors they can rely on from **any** provider.
2. Know which features require a capability check before calling.
3. Detect provider regressions early — not at runtime against Clockwork tasks.

Today, adapter tests are per-package with no shared behavioral baseline. There is no
authoritative list of required vs optional behavior. There is no machine-readable
capability declaration. This ADR defines both the compliance matrix and the testing
architecture that enforces it.

---

## Decision

### 1. Capability Declarations

Each provider declares its capabilities in a `Capabilities` struct returned by the
`Caps()` method added to `provider.Runtime`:

```go
// Capabilities declares what a Runtime's Sessions support beyond the
// baseline contract. All fields are false/zero by default — adapters
// opt in. Callers must check before using the corresponding affordance.
type Capabilities struct {
    // PTY: Session.SendInput writes to a live PTY master. Resize is meaningful.
    // False: turn-based adapters (opencode, claudestream, goprovider, stub).
    PTY bool

    // Resize: Session.Resize has observable effect. Requires PTY=true to be useful.
    Resize bool

    // ProviderSessionID: the adapter observes and stores a provider-side session ID
    // (e.g. claude --session, opencode sessionID) for cross-session continuity.
    // Exposed via ProviderSessionID() string on the Session (type-assert required).
    ProviderSessionID bool

    // CheckpointResume: Session.CheckpointHints returns a non-trivial hint.
    // Consumers may use it for cross-session continuity beyond ProviderSessionID.
    CheckpointResume bool

    // BinaryRequired: Prepare will fail if the provider binary is absent.
    // False only for in-process providers (api-stub).
    BinaryRequired bool
}
```

**Interface change to `provider.Runtime`:**

```go
type Runtime interface {
    ID()   string
    Kind() RuntimeKind
    Caps() Capabilities                                              // NEW
    Prepare(ctx context.Context, plan *launch.Plan) error
    Start(ctx context.Context, plan *launch.Plan, opts StartOptions) (Session, error)
}
```

**Capability matrix by adapter:**

| Adapter       | Kind | PTY   | Resize | ProviderSessionID | CheckpointResume | BinaryRequired |
|---------------|------|-------|--------|-------------------|------------------|----------------|
| claude-code   | cli  | true  | true   | false             | false            | true           |
| claude-stream | cli  | false | false  | true              | false            | true           |
| opencode      | cli  | false | false  | true              | false            | true           |
| goprovider    | cli  | false | false  | true              | false            | true           |
| api-stub      | api  | false | false  | false             | false            | false          |

### 2. Compliance Test Suite

A shared compliance suite lives in `internal/provider/compliance/`. It contains:

- `Suite` — a test function that exercises the required behavioral baseline.
- `CapsSuite` — a test function that exercises optional capability-gated behaviors.
- `ProviderHarness` — a factory type adapters supply to wire a real or fake Runtime.

#### 2a. Required Baseline (all adapters)

These tests MUST pass for any adapter — they are unconditional. The suite runs
them against every adapter regardless of capabilities:

| Test | Behavior verified |
|------|-------------------|
| `TestPrepare_EmptyCommandErrors` | `Prepare` with empty command returns non-nil error. (Skipped for BinaryRequired=false.) |
| `TestStart_ProducesAliveSession` | `Start` returns a session; `Health().Alive == true` immediately. |
| `TestHealth_AliveAfterStart` | `Health().Alive` is true on a freshly started session. |
| `TestHealth_DeadAfterStop` | `Health().Alive` is false after `Stop`. |
| `TestStop_UnblocksWait` | `Stop` causes `Wait` to return within a deadline. |
| `TestStop_IsIdempotent` | Calling `Stop` twice does not panic or return error. |
| `TestSendInput_AfterStop_ReturnsErrNoInputChannel` | `SendInput` after `Stop` returns `provider.ErrNoInputChannel`. |
| `TestWait_ReturnsExitCode` | `Wait` returns an integer exit code (any value is accepted; signaling is provider-defined). |
| `TestInterfaceConformance` | Static: `_ provider.Runtime = rt; _ provider.Session = sess` (compile-time, validated by each adapter package already). |

#### 2b. Optional Capability Tests (gated)

These tests run only when the adapter's `Caps()` declares the capability.
Skipping an optional test is not a compliance failure.

| Capability | Test | Behavior verified |
|------------|------|-------------------|
| `PTY=true` | `TestSendInput_WritesToPTY` | SendInput bytes appear on fanout (or the session executes them). |
| `Resize=true` | `TestResize_DoesNotError` | Resize with valid rows/cols returns nil. |
| `Resize=true` | `TestResize_ZeroDimensionErrors` | Resize with 0×0 returns error (PTY syscall would reject it). |
| `ProviderSessionID=true` | `TestProviderSessionID_CapturedAfterTurn` | After SendInput + turn completion, `sess.(SessionIDer).ProviderSessionID()` returns non-empty. |
| `ProviderSessionID=true` | `TestProviderSessionID_PresetCarried` | When `StartOptions.ClaudeSessionIDPreset` is set, `ProviderSessionID()` returns it before any turn. |
| `BinaryRequired=false` | `TestPrepare_NoopWhenNoBinary` | `Prepare` with nil/empty plan does not error (stub path). |
| `CheckpointResume=true` | `TestCheckpointHints_NonTrivial` | `CheckpointHints()` returns `(_, true)`. |

#### 2c. Test binary gating

Real-provider tests (claude, opencode) require external binaries. The suite
skips them via a `BinarySkipper` hook passed in the harness:

```go
type ProviderHarness struct {
    // NewRuntime returns the Runtime under test. May be a real binary-backed
    // adapter or a fake. If nil, the test is skipped.
    NewRuntime func(t *testing.T) provider.Runtime

    // NewPlan returns a launch.Plan suitable for this runtime.
    // Minimal: the plan only needs Command set.
    NewPlan func(t *testing.T) *launch.Plan

    // BinarySkip, when true, skips tests that require the real binary.
    // Adapters whose binary is unavailable in CI set this via exec.LookPath.
    BinarySkip bool
}
```

Adapters that don't have their binary available at test time use a fake
(e.g. `/bin/true` or `sh -c 'echo JSON'`) so lifecycle tests still run,
but mark `BinarySkip=true` to suppress tests that validate real behavior.

### 3. Runtime Health Signal Surface

Health reporting is currently a point-in-time `Health() HealthStatus` call.
Consumers (Clockwork) need a richer, queryable signal. ADR 0026 governs the
implementation; this section defines the design contract.

#### 3a. Enriched HealthStatus

```go
// HealthStatus is the observable runtime snapshot of a live session.
// PID is meaningful only for PTY-backed CLI runtimes. API-backed sessions
// report PID=0. State disambiguates the three coarse-grained sub-states
// within a "running" Mux session that both PTY and turn-based adapters traverse.
type HealthStatus struct {
    Alive  bool
    PID    int        // 0 for non-PTY adapters
    State  LiveState  // NEW: fine-grained within-running state
    TurnID string     // NEW: opaque string set by turn-based adapters during a live turn
}

// LiveState describes the fine-grained state of a running session as
// observed by the provider adapter. Consumers may use it for scheduling
// decisions (e.g. Clockwork deciding whether to send another turn).
type LiveState int

const (
    // LiveStateIdle: session is alive and waiting for input. No turn in flight.
    LiveStateIdle LiveState = iota
    // LiveStateProcessing: a turn or subprocess is currently running.
    LiveStateProcessing
    // LiveStateStopped: Stop has been called; Wait will return soon.
    LiveStateStopped
)
```

**Why `LiveState` instead of just `Alive`:**
- PTY sessions are always in `LiveStateIdle` or `LiveStateStopped` (no turn
  concept at the provider level — the PTY is always live).
- Turn-based sessions (claudestream, opencode, goprovider) need to distinguish
  "alive and idle" from "alive and processing a turn" without callers polling
  for `ErrTurnInFlight`.
- `LiveStateStopped` makes the terminal transition observable before `Wait`
  returns, enabling consumers to distinguish "session is draining" from
  "session was never started".

#### 3b. Health endpoint (`GET /sessions/{id}/health`)

A new HTTP endpoint exposes the runtime health as a JSON snapshot:

```json
{
  "session_id": "...",
  "alive": true,
  "pid": 12345,
  "live_state": "processing",
  "turn_id": "turn-7f3a",
  "provider_id": "claude-stream",
  "provider_kind": "cli",
  "caps": {
    "pty": false,
    "resize": false,
    "provider_session_id": true,
    "checkpoint_resume": false,
    "binary_required": true
  }
}
```

- Returns `404` if the session does not exist.
- Returns `409` if the session is not in state `running` (created/launched
  sessions have no live runtime yet).
- The `caps` field is derived from `runtime.Caps()` and is stable for the
  session's lifetime.

#### 3c. MCP tool

A new `mux_session_health(session_id)` MCP tool mirrors the HTTP endpoint:

```json
{
  "ok": true,
  "data": { ...same shape as HTTP response... }
}
```

---

## Compliance / Smoke Story

The Phase 4 compliance check is: run `go test ./internal/provider/compliance/...`
against all adapters. Each adapter package invokes `compliance.Run(t, harness)` in a
`TestCompliance` function. CI passes when all baseline tests pass and all
capability-gated tests pass on adapters that declare those capabilities.

The health endpoint is verified by the same smoke story:
- Start a session against api-stub.
- Poll `GET /sessions/{id}/health` — expect `alive=true, live_state=idle`.
- Call `SendInput`.
- Poll again — expect `live_state=processing` (adapter emits it).
- Call `Stop`.
- Poll once more — expect `alive=false` or a `409` from the finished session.

---

## Non-Goals

- Every provider does NOT need to support every capability. Unsupported capabilities
  must not return false success — they must be declared false in `Caps()` and the
  corresponding test must be skipped.
- Clockwork scheduling decisions are NOT added to Mux health. `LiveState` is a
  provider-observable signal; what Clockwork does with it is Clockwork's concern.
- No provider-specific health endpoints. The single `GET /sessions/{id}/health`
  surface is adapter-agnostic.

---

## Consequences

- **`provider.Runtime` gains one method (`Caps()`).** This is an additive, non-
  breaking interface change. All existing adapters gain a `Caps()` method that
  returns their static capability struct. Downstream consumers that embed `Runtime`
  must implement `Caps()` — the compiler will report this.
- **`provider.HealthStatus` gains two fields (`State`, `TurnID`).** Existing callers
  that construct `HealthStatus` structs (tests, adapters) are not broken — struct
  literals with named fields continue to compile; the new fields default to zero values.
- **A shared compliance package at `internal/provider/compliance/` is new.** It has
  no external dependencies and does not import any adapter package (adapters import it).
- **A `GET /sessions/{id}/health` HTTP endpoint and `mux_session_health` MCP tool
  are new.** They are additive only; no existing endpoints change.
- **`LiveState` strings** (`idle`, `processing`, `stopped`) are stable API surface
  once 0026 ships. Changes require a new ADR.

---

## Implementation Notes for CW-20260423-0025 (Compliance Suite)

1. Add `Caps() Capabilities` to `provider.Runtime` interface in `internal/provider/provider.go`.
2. Add `LiveState` type and `TurnID` field to `HealthStatus` in `provider.go`.
3. Add `Capabilities` struct to `provider.go`.
4. Implement `Caps()` on all five adapters (return the matrix above).
5. Create `internal/provider/compliance/compliance.go` with `Suite`, `CapsSuite`, `ProviderHarness`.
6. Add `TestCompliance(t)` to each adapter's `_test.go` using the harness.
7. All baseline tests must pass; gated tests skip when `BinarySkip=true`.

## Implementation Notes for CW-20260423-0026 (Runtime Health Signal)

1. Update `HealthStatus` to include `State LiveState` and `TurnID string` (step 2 above is shared).
2. Add `LiveStateIdle`, `LiveStateProcessing`, `LiveStateStopped` constants.
3. Update all adapters to set `State` correctly in `Health()`:
   - `claude-code` (PTY): always `LiveStateIdle` (PTY is always live).
   - `claude-stream`, `opencode`, `goprovider`: `LiveStateProcessing` when `current != nil`, else `LiveStateIdle`.
   - `api-stub`: `LiveStateIdle` (no turn concept).
   - All: `LiveStateStopped` when `stopped == true`.
4. Add `GET /sessions/{id}/health` handler in `internal/api/`.
5. Add `mux_session_health` MCP tool in `internal/mcpadapter/`.
6. Include `caps` object (from `runtime.Caps()`) in both responses.
7. Smoke tests: compliance/smoke story described above.

---

## Related ADRs

- [ADR 0006](0006-provider-contract-shape.md) — Runtime + Session interface split (superseded in part)
- [ADR 0014](0014-pty-resize-endpoint.md) — PTY resize contract
- [ADR 0022](0022-mux-as-optional-provider-substrate.md) — Provider/session contract for Clockwork + Nanite
