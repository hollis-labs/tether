# Verified definitions and resolver

This is an inert implementation of the ADR 0054 definition boundary and the
`mesh.Resolver` interface. No daemon, route, MCP tool or launch path uses it.
The existing enrollment and registration APIs are unchanged.

The definition store uses `mesh/agentdef` strict parsing and validation, a
host-owned capability vocabulary, and explicit namespace/version extension
handlers. Every known handler must validate its real schema and enumerate typed
content references. Unsupported mandatory extensions fail. Unknown optional
extensions remain uninterpreted and participate in the semantic digest; they
cannot grant executable semantics. All core pinned references are verified,
including complete skill packages and capability input/output contracts.
Permission profile names are pinned semantic data, not grants or mode bindings.

Indexing stores references, semantic digests and exact artifact digests in the
existing fabric tables, never authored bodies or resource configuration. A
revision cannot be reassigned to different semantics. A presentation-only change
needs an explicit new artifact index entry before resolution accepts it. Loads
verify the original indexed source and all dependencies every time; missing or
changed content fails, with no latest lookup or alternate-source fallback.

The local provider accepts only `catalog:` relative paths beneath one root that
the host authorizes and canonicalizes. It keeps an `os.Root` descriptor, rejects
links beneath that root and special files, and uses no-follow, nonblocking file
opening on Unix. Other platforms fail closed for this provider. Definitions are
bounded at 4 MiB; dependency content at 64 MiB per pin, 10,000 files, 20,000
members and 64 member path components. Cancellation and file replacement checks
apply. These limits are host implementation limits, not changes to the protocol.

`catalog:` binds to **agentdef-content-v1**. Hash exact file bytes for each
content hash; a file root uses the synthetic path `content`. A tree includes
all regular files, including dotfiles. Sort slash-separated paths by raw UTF-8
bytes. Hash a stream containing the protocol name plus LF, the kind (`file` or
`tree`) plus LF, then one line per file: lowercase SHA-256 content hex, TAB,
owner-executable flag (`0` or `1`, mode bit 0100), TAB, relative path, LF. Prefix
the outer SHA-256 hex with `sha256:`. A file pin therefore differs from sha256sum
of that file. Empty directories and unrelated metadata do not enter the stream. Checkout line-ending conversion changes the pinned bytes and invalidates the pin.

Paths must be nonempty UTF-8 NFC; reject rather than normalize. Absolute paths,
backslashes, colons, controls, empty components, dot and parent components fail.
Every member is validated, including empty directories. Unicode default case
folding followed by NFC detects collisions among members; folding does not
change hash ordering. A future protocol needs a different URI scheme or schema
version, rather than reinterpreting existing pins. Tests reproduce the five
portable vectors and refusal cases with synthetic temporary fixtures.

The provider requires stable authored-tree ownership during verification and
use. Confinement and replacement checks do not turn a mutable path into an
immutable capability, and the resolver does not materialize resources or confer
permission to reopen them later. The host must hold the corresponding ownership
or use immutable content when it eventually consumes those resources.

Resolution requires host authorization. It reads enrollment, session, instance
and binding records consistently, verifies current and historical session pins,
and rechecks record versions after I/O. An unexpired binding for another requested
session is an explicit error. Expired bindings are not reported as current and
are not released as a side effect. Resolution does not enroll, acquire/renew
leases, launch, or decide that an expired process is dead. Launch admission must
revalidate authority independently. The root module pins mesh v0.1.0. This package
remains unwired; deployment and activation are separate host decisions.
