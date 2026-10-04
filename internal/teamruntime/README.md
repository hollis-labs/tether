# Team runtime adapters

The daemon constructs these adapters only under `teams.enabled`, default false.
Construction performs no enrollment, launch, publication, recovery or background
work. HTTP and native MCP share one service over the daemon's existing migrated
database and runtime. Normal launch and message methods remain available without
constructing any team component. The daemon does not schedule
`Reconciler.Reconcile`; formation refuses with empty trust and execution targets
until activation readiness is resolved in CW-20261004-0002.

`LegacyEnroller` implements the entire enrollment port over the legacy registry.
It also owns the current session launch-target lookup, binding attachment and
authenticated-session actor lookup through narrow interfaces. Only this adapter
touches legacy registry or runtime-binding persistence, including receipt recovery;
Sessions and Principals do not. The later fabric enrollment swap replaces this
one adapter. Identity admission, enforce-mode refusal and local-operator proof
remain in Principals, so an enrollment swap cannot omit those guards.
Fresh identities are minted by `RegisterIdempotent`. The receipt commits a
server-generated random nonce before registration; its external ID and provenance
property use that nonce. A second independently random binding secret is
committed in the receipt and never placed in enrollment props or external IDs.
Only it authorizes the binding attempt; adoption also requires TetherHosted
visibility. The public enrollment nonce cannot authorize binding attachment or cleanup. Namespaced names and predictable intent keys prove no
ownership. Ensure verifies the provenance, pin, active status and agent kind. Pool/durable identities must already be active
registry agents; their public `Props["team_definition"]` holds only the exact
DefinitionRef JSON, never authored content, runtime configuration or secrets.
The definition verifier checks the original indexed source and pinned references.
The public team_definition property is pin metadata, not proof of ownership.
Operator-supplied exact-pin execution targets select existing legacy catalog
launches. No target or verifier omission grants admission. A target is a trusted
host binding to the legacy launch configuration; it must be maintained alongside
that catalog until the fabric enrollment adapter replaces this port.

Migration adds one receipt table. Request and key commit before any effect;
completion commits afterwards. Registry nonce keys, random session idempotency keys (`team-port:<nonce>`)
and message queue keys recover effects after a lost acknowledgement. End and
binding tombstones commit before cleanup and permanently fence late calls.
Cleanup revokes only this intent's binding and retires only its own fresh registry
entry; stable identities remain enrolled. Team-vs-team exclusivity is guarded by host actor reservations and the
adapter lease transaction. Generic registry consumers keep their existing
behavior; cross-consumer exclusivity must be decided at the gated activation boundary. Session launch replaces that lease's
reservation session with its exact acquired session, without minting another
binding. Plans retain the catalog logical agent for sandbox and launch-policy
resolution; an internal TeamMember marker skips ordinary actor mirrors and home
binding acquisition. Stop acknowledges only
an observed terminal state and serializes with ordinary launches. Detached and
unavailable runtimes remain retryable. Missing acquisitions still retain their
intent tombstone, so Stop absence cannot permit a late launch.

The daemon is the sole runtime writer. Keyed effect locks span adapter instances
sharing its database handle; no SQL transaction spans a process/registry call.
Recovery uses a durable rotating insertion cursor and bounded output pages. It
acts ONLY on receipt rows: no namespace, prefix, metadata or host-id sweep may
adopt or clean foreign objects. Write-ahead ordering makes unreceipted team effects
impossible. Cleanup retires only the recorded URN with matching provenance;
stable identities stay enrolled. Session completion failures compensate only
when the receipt ended. The host alone drives its delivery outbox, so recovery
never re-drives a dead-lettered, dispatched or terminal-delegation delivery.
Permanent acquisition/delivery refusals are terminal receipts.

Receipts are retained indefinitely; no pruning implementation exists. AUTOINCREMENT
prevents sequence reuse, including after deletion. The pending index supports
non-delivery recovery candidates, but output limits do not bound every SQL scan. Scheduling
Reconcile is gated on the retention and query-bounding follow-up, including the
host retained-session lookup/clamp and authenticated-session actor query. The
former messages LIKE scan is removed entirely. Effect locks are also a retained
global map; context-aware bounded lock lifetime remains activation work.

Agent messages use the existing durable idle queue and its dispatcher. The
recipient session is retained and no successor actor is supplied. Detached
sessions retry; ended sessions refuse. Unsupported policies and runtimes without
an idle boundary refuse. Sessionless user/service recipients receive a keyed
mailbox message through the existing reliable delivery store: this queues content
for their own pull and injects no runtime turn. Keyed mailbox retries repair a
content write that failed after delivery enqueue. Full requests bind port keys,
including sender, body, recipient, session, policy and retained route. Channel
naming returns `team.<run>` and creates nothing; normal first publication creates
the implicit channel.

`Principals` requires enforce mode and reads only middleware-verified context.
Observe/off attribution cannot authorize a team verb. A verified team session
resolves its actor through its retained receipt and exact current binding. An
ordinary verified agent session resolves its existing logical actor through the
same exact current-binding check; it cannot borrow a successor session.
Verified services and interactive users keep their authenticated actor address.
The reserved operator principal requires both its verified operator credential
and an accepted Unix socket context, supplied by `identity.ConnectionContext`;
headers, query selectors, request bodies and a socket alone prove nothing. The
later daemon/native MCP wiring must preserve that connection context. With a
`unix:` listener every accepted caller is a *net.UnixConn, so the local operator
proof currently reduces to its verified bearer credential. No peer-UID check
exists yet; adding it is an activation follow-up.

The ownership test exempts exactly legacy_enrollment.go and legacy_sessions.go
and checks registry imports plus runtime_bindings and registry_ SQL literals. It
is a best-effort literal guard: concatenated SQL can evade it.

Provision limits are validated and retained on host members, but budget/timeout
are not enforced by session Start here. Enforce them before activation, alongside
recording original granted scopes for recovery launches.

Team sessions retain the catalog logical_agent_id for sandbox policy, while
skipping the ordinary actor binding lease. Legacy ordinary lookups still key on
that logical ID alone: legacyNewestRunningSession in wake.go can select a team
session, as can GetLatestCheckpointForAgent and DetachedSessionIDForAgent for
ordinary resume. Separating those lookups is required before activation.
Binding-only receipts may also remain eligible on every recovery pass; their
completed cleanup needs a separate retained marker before scheduling recovery.

This package has no daemon registration, HTTP/MCP routes, CLI commands, config
activation, restart or deployment. Those belong to the later composition slice.
