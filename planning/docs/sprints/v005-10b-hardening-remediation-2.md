# Sprint v005-10b — Hardening Remediation Sprint 2

**Epic:** Mux beta-readiness push
**Status:** staged (follow-up to v005-10)
**Branch:** `feat/v005-10b-hardening-remediation-2`
**Dependency:** v005-10 (hardening audit + P0 remediation) merged to `main`
**Date staged:** 2026-05-12

---

## Goal

Execute the P1 items captured by the v005-10 hardening audit. P0 shipped in v005-10; this sprint clears the P1 list so beta enters with no known load-bearing hardening gaps.

Source of truth for the punch list: `agent-workspaces/execution/agent-mux/v005-10-hardening-audit/2026-05-12/audit-findings.md` §9.

---

## Scope (P1 items from v005-10 audit)

### Code structure

- [ ] **T-v005-s10b-01 — P1-A.** Split `internal/client/client.go` (685 LOC, 33 funcs, 28 public methods) by domain: `client_sessions.go`, `client_messages.go`, `client_catalog.go`, `client_proxy.go`. Single `Client` struct preserved; mechanical move.
- [ ] **T-v005-s10b-02 — P1-B.** Extract per-phase helpers from `internal/mcpadapter/proxy_adapter.go:RunWithProxyOpts` (144 LOC): `bootProxyClients`, `registerNativeServers`, `registerDiscoverAndCall`.
- [ ] **T-v005-s10b-03 — P1-C.** Verb-split `internal/api/sessions.go:handleSessionsItem` (120 LOC) into `handleSessionsGet` / `handleSessionsPatch` / etc.
- [ ] **T-v005-s10b-04 — P1-E.** Extract `setupListeners` + `shutdownSequence` from `internal/daemon/server.go:Server.Run` (97 LOC).
- [ ] **T-v005-s10b-05 — P1-F.** Case-by-case helper extraction on remaining 80+ LOC funcs: `registerDiscoverTool`, `DiscoveryIndex.Search`, `runMCP`, `runAttach`, `ListEnvelopesByWorkflow`, `handleSessionCreate`, `QueryProxyEvents`, `registerSessionEventsTool`, `registerMCPServersTool`, `handleBrokerRequests`. Audit each — split only where it improves clarity, leave-with-reason otherwise.

### Test coverage uplift

- [ ] **T-v005-s10b-06 — P1-G.** `internal/mcpadapter` (26.0%) — isolate deterministic surface (ToolRegistry routing, DiscoveryIndex matching, ProxyRouter routing decisions) from subprocess code paths so unit tests can exercise them without spawning MCP servers.
- [ ] **T-v005-s10b-07 — P1-H.** `cmd/mux` (12.3%) — extract pure helpers from cobra glue into `cmd/mux/<feature>_internal.go` so they're testable in-package. Leave the cobra plumbing itself untested (structural).
- [ ] **T-v005-s10b-08 — P1-I.** `internal/client` (42.6%) — pair with P1-A split; add per-domain tests for newly extracted client files.
- [ ] **T-v005-s10b-09 — Coverage hard gate.** Pick a threshold from the v005-10 baseline (50.7% pre-acpsvc-tests, 52.8% post). Suggested floor: 55% to start, ratchet upward each sprint. Fail `make check` below threshold.

### Error-handling discipline

- [ ] **T-v005-s10b-10 — P1-J.** Add `slog.Warn` on the 4 silent `_ = bus.Publish(...)` sites: `internal/app/sinks.go:113`, `:154`, `internal/broker/service.go:114`, `internal/daemon/server.go:78`.
- [ ] **T-v005-s10b-11 — P1-K.** Add `slog.Debug` on the 7 silent write-error sites: `internal/acpadapter/dispatcher.go:126/135/142/145/148`, `internal/daemon/server.go:265`, `internal/api/errors.go:38`.
- [ ] **T-v005-s10b-12 — P2-D.** Log the 2 sites where `_ = s.Store.UpdateSessionState(..., "failed", ...)` silently swallows: `internal/app/service.go` (now lives in `session_lifecycle.go` after v005-10 P0-A split) compound-failure paths.

### Dependency hygiene + security

- [ ] **T-v005-s10b-13 — P1-L.** Bulk dep refresh — read changelogs first, especially: `mark3labs/mcp-go v0.47.0 → v0.53.0` (6 minors), portfolio libs (go-llm-contracts/go-llm-types v0.1 → v0.2, go-runner v0.4 → v0.5, go-sandbox v0.2.0 → v0.2.1, go-messaging v0.2.0 → v0.2.1, go-mcp-sanitize v0.1.0 → v0.1.1). Pin in `go.mod`; run full gate after each major bump.
- [ ] **T-v005-s10b-14 — P1-M.** Decide bootgen `cmd:` slot kind security model for Tier-2 caller-provided boot profiles. Three options captured in v005-10 audit §6: disallow `cmd:` in Tier-2 / require filesystem-locked paths / accept-as-documented. ADR-worthy.
- [ ] **T-v005-s10b-15 — P1-N.** Cap `internal/client/client.go:624` `io.ReadAll` with `io.LimitReader` (16 MiB suggested).

### Portfolio invariants

- [ ] **T-v005-s10b-16 — P2-H.** Audit `goleak.VerifyTestMain` discipline across goroutine-spawning packages: `internal/events`, `internal/daemon`, `internal/broker`, `internal/mcpadapter`, `internal/acpsvc`, `internal/app`. Add where missing.
- [ ] **T-v005-s10b-17 — P2-I.** Verify OTEL exporter presence per `feedback_go_ecosystem_baseline`. Wire if absent or document the gap explicitly.

### CI / tooling

- [ ] **T-v005-s10b-18 — P2-J.** Wire Codecov (or equivalent) for coverage trend visibility.
- [ ] **T-v005-s10b-19 — P2-K.** Add Renovate or Dependabot for dep update PRs.

### Documentation

- [ ] **T-v005-s10b-20 — P2-L.** Optional: add a CodeQL workflow (gosec already covers most surfaces). Defer if low signal.
- [ ] **T-v005-s10b-21 — P2-F.** Add package-level note in `internal/mcpadapter/middleware.go` referencing ADR 0021 Phase 3 — the dead-code finder flags scaffolding funcs as unreachable today; the note documents intent so future audits don't re-discover.

---

## Out of scope (deferred again)

- All P2 items not lifted into the P1 list above. Re-audit at v005-12 (or end of beta) and reclassify if any have become P0/P1 in the meantime.
- Build/CI overhaul beyond the specific gaps above.

---

## Acceptance

- [ ] P1 items executed (or explicitly deferred with reason in the closing report).
- [ ] Coverage hard gate landed in `make check` (T-09).
- [ ] ADR 0037+ for any cross-cutting decisions surfaced during execution.
- [ ] `make check` green.
- [ ] FF-merged; branch deleted; parent boot prompt updated.

---

## Notes

- The audit-findings file in `agent-workspaces/execution/agent-mux/v005-10-hardening-audit/2026-05-12/audit-findings.md` is the canonical detail source for every item here — link to its sections by ID (P1-A, P1-B, etc.) when implementing.
- Capture-and-defer is the default for any sub-finding that grows past a sprint commit cluster. Don't expand scope inside a P1 item; spin a v005-10c if needed.
