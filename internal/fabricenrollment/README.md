# Private enrollment kernel

This package is an inert host implementation of ADR 0054 and ADR 0060. Nothing
in the daemon, registry API, CLI or MCP composition calls it. Existing legacy
registration and lookup behavior is unchanged. It performs no live import,
remote enrollment, process launch, lease acquisition or identity merge.

The host supplies authenticated caller authorization for each action and owner,
a verifying definition store, an explicit directory publication policy and a
clock. Owner references and pins are data, never grants. Enroll creates actors of
the shared mesh kinds; only agents bind verified definition revisions. Existing
identities cannot be overwritten or promoted. Rebind uses the expected enrollment
version, changes only future sessions and requires an explicitly released binding.
Even an expired stored lease refuses rebind or retirement: expiry is not proof
of process death. Retirement preserves URNs and history and cannot reactivate.
Each mutation and its redacted durable outbox event commit in one transaction.
This package does not dispatch that outbox.

Directory queries first authorize the entire owner-scoped identity index, then
check row visibility and explicit publication policy. Enrollment alone publishes
nothing. A private projection contains mesh identity/kind/lifecycle, a pin,
filtered offered service IDs, record revision and bounded verification freshness.
It exposes no owner, content, policy, source path, locator or execution claim.
The host must not advertise an ID absent from the verified definition. The
projection uses current verified pins and checks record versions after content
I/O. Its cursor is private owner-scope pagination state and can cover unpublished
identities in that authorized scope; a future route must not publish it as agent
metadata. This is a Tether host type, not a second shared directory wire contract.

Import input is an explicitly supplied identity-only snapshot and reviewed
mapping manifest. No reader of a live registry or catalog exists here. A source
key maps once to its exact original URN and, for an agent, a verified indexed pin.
Duplicate source keys, duplicate identities, multiple mappings, extra mappings,
kind mismatches and URN changes refuse. Missing mappings produce unresolved
preview problems; apply refuses the whole batch. Existing enrolled identities
are not merged, even if their pins appear to match.

Preview is read-only. Apply writes enrollments, reviewed candidates, immutable
receipts, provenance references and outbox entries together, or rolls everything
back. An approval reference records evidence; it does not bypass authorization.
Retries are keyed by the computed source snapshot and mapping digests, preserve
the first receipt and remain authorized even after the identity is retired.
They never reinterpret changed content or create a second identity. The manifest
used in a real adoption is a later apply-time input; tests use synthetic data.

The import digest scheme is internal provenance, separate from the shared content
pin protocol. Copy and byte-sort snapshot identities and mappings by source key,
then SHA-256 the version string plus LF and compact Go JSON of the declared input
structs (no terminal LF). Versions are `fabric-import-source-v1` and
`fabric-import-mapping-v1`. Receipt IDs hash `fabric-import-receipt-v1` plus LF
and a JSON array of source digest, mapping digest and source key. Prefix each
hex digest with `sha256:`. No name, URN or path normalization occurs. Import is
bounded at 100 identities; there is no partial-batch resume or automatic matching.
