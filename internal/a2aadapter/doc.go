// Package a2aadapter implements T10 (messaging vNext, CW-20260906-0041): "a
// bounded, optional, explicitly namespaced A2A adapter" over the canonical
// messaging service, using the official A2A (Agent2Agent) protocol's
// maintained Go SDK, github.com/a2aproject/a2a-go/v2 (protocol spec v1.0,
// stable since April 2026; SDK v2.5.0 verified current as of this task's
// execution).
//
// Scope, locked by design decision on 2026-09-07 (Chrispian, in response to
// an explicit scope-fork question): server-only inbound relay. Tether
// exposes selected registered agents as A2A servers so an external A2A
// peer can message them via the standard protocol; Tether never initiates
// an outbound A2A call. The deferred outbound-client half is tracked as a
// backlog task (CW-20260907-0028), not implemented here.
//
// # Bounded, not a task engine
//
// The architecture explicitly warns: "Tether does not become a Torque task
// executor, team engine or external lifecycle controller." This package
// honors that literally: TetherExecutor (executor.go) never decides a
// delegated task's outcome itself. Every inbound A2A message — message-only
// or delegated-work — is relayed through the SAME canonical, delivery-backed
// messaging service the rest of Tether uses (internal/store's
// InboxStore.Send, the T03 delivery core), so a Tether-side consumer sees it
// exactly like any other incoming message and can inspect/trace/redrive it
// with the T09 tooling. A delegated task's actual completion is signaled
// back ONLY through a typed, explicitly authorized HTTP call
// (transition.go's POST .../tasks/{id}/transition, matching T09's
// authorized_by convention) — Tether relays and waits; a consumer decides
// and calls back.
//
// # ID discipline (T10 acceptance #2)
//
// A2A Message/Context/Task IDs are carried ONLY as envelope metadata
// (a2a_message_id / a2a_context_id / a2a_task_id) on the canonical Tether
// message — never as the Tether message ID, and never as a substitute for
// an AgentID or SESSION. The external peer itself is addressed as a Tether
// messaging.KindService identity (msg://service/a2a/<binding-id>), not
// folded into Tether's own agent/session identity namespaces.
//
// # Generic messages are not forced into tasks (T10 acceptance #2)
//
// Whether an inbound message becomes a task is an explicit, per-binding
// operator decision (AgentBinding.TaskMode in config.go), not something
// this package infers from message content. A TaskMode=false binding never
// emits an a2a.Task event — every message stays a plain message-only
// exchange, by construction.
//
// # Unsupported operations are explicit (T10 acceptance #3)
//
// Every binding declares a2a.AgentCapabilities{Streaming: false,
// PushNotifications: false} and wires a2asrv.WithCapabilityChecks with it —
// the SDK itself then returns a2a.ErrUnsupportedOperation /
// a2a.ErrPushNotificationNotSupported for any client attempt to use a
// capability this adapter doesn't implement, rather than this package
// reinventing that signaling.
//
// # Retry/replay/auth ride the canonical service (T10 acceptance #3)
//
// This package adds no parallel retry, idempotency, or delivery-obligation
// bookkeeping of its own: every relayed message goes through
// InboxStore.Send, so it gets the same durable delivery/attempt/receipt
// tracking (and the same T09 trace/redrive tooling) as a locally-sent
// Tether message. Auth is a pluggable a2asrv.CallInterceptor
// (auth.go's BearerTokenInterceptor) checked before a request ever reaches
// the executor — not a bespoke scheme layered on top of the protocol.
//
// # Fixture-only (per this task's own scope text)
//
// Interop tests in this package stand up a real a2a-go client against a
// real Adapter-served httptest.Server in-process — genuine SDK-to-SDK
// protocol conformance, never a live network deployment.
package a2aadapter
