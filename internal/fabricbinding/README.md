# Private binding kernel

This is an inert host implementation of ADR 0054 and ADR 0060. It uses the
existing fabric tables. No Manager.Start, running leaseActorBinding, daemon,
registry endpoint, CLI or MCP route calls it. It launches no process, reconnects
no shim, dispatches no outbox and runs no live import.

Admit authorizes the authenticated caller, owner, agent, requested execution and
content references, and pinned definition. Host authorization owns delegation
and reference access; input references are never grants. Existing sessions keep
their original definition/context/store pin. Content verification occurs before
the writer; the agent and session versions are checked again inside the write.
Session, instance, binding, immutable acquisition receipt, caller-scoped admission
and outbox commit together. No transaction callback is retried.

The full agent URN keys one binding head. Every acquisition advances its durable
high-water fence, including after explicit release or expiry. Two independent DB
connections cannot acquire the same live identity. Expiry permits fenced
supersession but does not declare the old process dead or rewrite its work state.
Waiting work and paused/detached sessions retain a live lease. The host remains
responsible for process quiescence and for fencing every external content/control
writer before deployment; this package does not confer that capability on a
content provider or running process.

Idempotency is scoped to caller plus key and an immutable request digest. An
identical retry returns the same reservation identity and original lease receipt
with current session/instance records. It never renews or reacquires the lease,
launches another instance, or grants present authority. Changed requests conflict.
An authorized retry remains a metadata read when source content is unavailable.

Renew, release, lifecycle and referenced-report ports check holder, complete
agent/session/instance identity, fence, head version and expiry inside the writer.
Lifecycle also compares session and instance versions. Terminal observations
conditionally release only their own current head; late old reports cannot release
or mutate a newer instance. Terminal instance history never reopens. Lifecycle
connectivity and work states remain separate shared mesh values. Release requires
host-authorized quiescence/death evidence and does not fabricate a terminal state.

The rejecting HostPort carries authenticated holder identity separately from the
observation, which carries the full binding identity and fence. Its mandatory
Validator is a pure, bounded policy check inside the writer: no I/O, side effects,
or reentrant repository use. Authorizer and Validator errors other than context
cancellation/deadline become opaque denied errors. Operational storage and content
faults propagate; none are silently treated as successful admission. Authoritative
registry transitions and durable outbox events commit together; no external sink
publication is claimed atomic with SQLite.

Clock readings precede content or policy work. Expiry is checked again inside the
writer and after report validation. Inputs stored or hashed must be valid UTF-8
and at most 4096 bytes per string, with positive bounded TTLs. Fences are restricted
to SQLite's positive signed integer range and refuse exhaustion without wrapping.
No identity or path normalization occurs.

Before activation the host must select TTL/recovery policy, authenticated ports,
content/control fencing and outbox delivery. This kernel establishes no public
API and no authority over the existing launch path.
