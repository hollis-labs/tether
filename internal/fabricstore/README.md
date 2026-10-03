# Fabric persistence

This package is an inert storage boundary for ADR 0054 enrollment and ADR 0060
sessions. It borrows the existing store database after migrations. Production
composition does not instantiate it yet.

Records retain the mesh wire snapshot as JSON alongside an optimistic version.
The stable identity is the primary key; compound keys are encoded as JSON arrays
to avoid ambiguous concatenation. Definition revisions, artifact provenance,
binding history and import receipts are append-only through the repository.
Session and instance pins remain fixed when the enrolled agent changes revision.
Only references and digests are stored for definitions and artifacts, never raw
catalog content or credentials. Digest verification belongs to the resolver.

`Write` reserves SQLite's writer with `BEGIN IMMEDIATE` before reading versions
and references. Its callback uses only the supplied transaction, and commits
all records and outbox events together. Returning an error rolls back everything.
Version zero creates a record; updates require the observed version. Missing
references, conflicting versions and invalid relationships are distinct errors.
Actor/agent consistency is checked before commit. References are validated here
because the existing database leaves foreign key enforcement off. Direct SQL
writes bypass these repository guarantees and must not be used by fabric hosts.

Binding heads retain their high-water mark after release. New holders require a
strictly larger fence, and values above SQLite's signed integer range fail.
This is not a lease acquisition API: admission policy must authenticate callers,
check expiry, reserve idempotent operations, and fence every authoritative write.
Expiry alone never proves process death. `Snapshot` reports persisted facts,
including the current head even when a different historical session is requested;
the resolver must verify pins and decide which lease is current.

Migration candidates and receipts store reviewed provenance without importing
legacy records automatically. Outbox cursors support ordered replay; marking an
event delivered does not promise exactly-once delivery to another system.

The initial dependency pins an exact substrate commit. A maintainer-approved
mesh release tag must replace it before runtime activation or deployment.
