# Team service

This package is the common service for future HTTP, MCP and CLI wrappers. It is
not registered with the daemon. All dependencies are injected; constructing it
requires authenticated-principal resolution, host formation ceilings, durable
run lookup and a caller-keyed journal in addition to the mesh host interfaces.

Request types carry no caller identity. `Principals` reads the daemon's trusted
authentication context. Unverified identities are refused, except for the
resolver's authenticated local operator. The operator URN is reserved: even a
verified identity with that URN is refused without the resolver's operator mark.
The host resolver may mark `LocalOperator` only after proving a local-socket
connection with the operator credential or peer identity; an asserted header or
request field never qualifies. The service accepts the resolver's verified
result and does not compute verification. Missing principal configuration yields
`ErrUnavailable`; resolver authentication and denial errors retain their typed
`ErrUnauthenticated` and `ErrDenied` classifications, while unknown or transient
failures yield `ErrUnavailable`, without exposing resolver details.
That exception grants no membership, permissions,
spawn capabilities or exemption from ceilings. Unknown members are masked as
not found. Team definitions and existing runs must use strict authority.

Formation accepts a definition and launch request, enforces explicit host bounds
and allowed grant verbs, and asks the host trust resolver to admit every pinned
slot. Nil ceilings fail construction; `ConservativePolicy` supplies a bounded
starting policy. Host round and stall ceilings must be positive; zero cannot
mean unlimited; malformed host ceilings yield `ErrUnavailable`. Launch counts, pool selections and request limits are validated
before storing a definition. A host must explicitly provide policy and trust;
no omitted trust decision permits admission. The caller becomes owner in the
authored phase owner slot, or coordinator slot. Missing both is invalid. An
external owner is a governance participant without a provisioned session or
spawn capability. The authored owner slot must leave room for that participant;
a full slot is refused rather than overfilled. The external owner shares that
slot routing pool and may receive slot-addressed work when every agent member
is busy. Owner governance permits administration, while message and
spawn grants still come from the team's authority table.

Membership, routing and results call the mesh library operations. Dissolution
uses its cancellation transaction as an aborting authorization probe over a
detached roster, then its trusted end-run operation over a guarded roster. This
keeps owner/admin/grant rules in the library and checks the entire run before
cleanup. A retry may use the saved actor only when every planned member is
already non-active; otherwise it must re-authorize the live caller. The guard
rejects new identities or sessions on a retained retry. If membership changes
between saving a plan and reserving it, that key conflicts; a new key is needed.
Cancellation plans are rechecked in the reservation transaction; recovery is
limited to the already-authorized targets and runs through library reconciliation.

Every mutation requires a key, scoped by principal and verb across runs. Reusing
a key with changed request content conflicts. `Calls` retains immutable request
bytes, the selected routing or termination plan, and one completed result.
`Ledger.WithLease` serializes and fences journal writes across service instances.
Messages save their plan before sending and retry through `SendResolved`; they
never select another worker after a partial delivery or lost acknowledgement.
Accepted delegation delivery keys in the result identify the original delegation
for assignee-only `ReportResult`, delivered to the original delegator at idle.
Completed retries still authenticate and require retained membership, but can
recover after a member has ended. Strangers are refused before acquiring journal
records. Keys are limited to 256 bytes, raw bodies and encoded team definitions to 64 KiB,
and encoded requests to 128 KiB. Results contain only member id, slot, actor,
status and kind; host intents, sessions and retained routes stay internal. A
sender must support the library delivery store for result acknowledgements.

The host owns journal retention, immutable roster-version retention, trust tiers,
identity enrollment, provision intent fencing and atomic delegation acceptance.
Run channel metadata stays in the library's format; transport channel naming is
the host's responsibility. Surfaces, migrations and production activation belong
to their adapters and are not implemented here.
