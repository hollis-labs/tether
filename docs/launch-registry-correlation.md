# Launcher correlation registration

Task-linked and workstream-linked local and remote-worker launches enqueue an
agency Registry `correlation_register` after a confirmed start. This client is
separate from Tether's federation directory (`internal/registry`). Registry owns
correlation identity; Tether never calls generic record creation or mints an
agency identity.

Configure the daemon's catalog `global.yaml`:

```yaml
agency_registry:
  url: https://registry.example.invalid
  credential_file: /absolute/protected/registry-launcher.credential
```

Cerberus provisions this private, daemon-owned file with `registry.write` and
`registry.correlation.register` scopes and the appropriate Registry authority
binding. This feature creates no credentials and changes no grants. The file
is read afresh on each attempt for rotation. Existing private-file ownership,
mode and symlink checks apply. The credential never enters a launch plan,
child environment, event, request body or log. Redirects are refused. Omitting
`url` disables outbound registration; a missing/bad credential fails only the
background registration. Configure and coordinate daemon cutover separately.

The wire contract is Registry `docs/minting.md` in `hollis-labs/tools/registry`:
`POST /v1/correlations/register`, JSON body of typed facts, with a stable
`Idempotency-Key: tether-launch:<session>:<execution-run>` header. Registry
permanently converges `(authority, session, run_id)` on one correlation. Every
retry sends identical facts and the same key; a lost success response cannot
mint a duplicate. HTTP 429, 5xx and transport failures retry; validation,
authorization and conflict errors stop. Response/error bodies are never logged.

Registration carries the run-token URN, canonical Tether session, execution run
ID, task ID, canonical Registry workstream reference, and verified token JTI
when available. `run_id` is `TETHER_EXECUTION_RUN_ID`, **not** the optional Torque
run/team ID. Only the opaque JTI is sent, never the run token. Old workers
without run-token support still have an execution run; their JTI stays absent.
A valid launch request tracing context supplies `traceparent`; ambient process
environment is never consulted for facts. The local launcher hashes committed
`.materialize/manifest.json` bytes as `sha256:<hex>` when present; it does not
hash boot contents or credentials. A known native resume/provider mapping adds
`provider_session_id`. Worker manifest receipts and late provider IDs are not
currently supplied by the launch acknowledgement and remain absent. Unknown
facts remain absent. Registration retries do not perform conditional late
enrichment, which is a separate Registry update contract.

There are at most 32 in-flight registrations per daemon, with three attempts,
a two-second deadline per attempt and 250/500 ms backoffs. No remote IO or
credential-file read runs in the launch critical path. A full queue drops the
registration and logs a value-free diagnostic. Permanent failures and exhausted
retries log a value-free diagnostic; neither changes session state or the launch
result. Shutdown cancels requests/backoffs and drains the bounded goroutines
before closing the store. This is an in-memory best-effort queue, not a durable
outbox: daemon crashes, overflow and long outages can lose registrations. It
never delays a launch to guarantee a remote receipt.

## Registry workstream references

Operator-admitted `workstream:<ULID>` values create/reuse a local reference row
under that exact key, atomically with session creation or launch-time filling.
No second public identity or Registry metadata copy is created. These rows
support lineage, digests and local project joins. String shape is not proof of
remote existence; the trusted dispatcher still supplies Registry-owned IDs.
The existing operator admission boundary remains in force across HTTP, MCP and
service launch entry points.

Task-linked unassigned lineages cannot use `EnsureSessionWorkstream` to mint a
local UUID or substitute a seed ID; they return an assignment-required error.
Unlinked standalone workstream creation remains compatible. Existing local IDs,
labels and historical joins are not rewritten. Legacy labels remain local and
are omitted from Registry's `workstream_id` fact; they are never claimed as
Registry identities. Descendants inherit the same reference and divergent
existing associations are not overwritten. A rollback to the preceding binary
can still read the rows, but loses reference creation and async registration;
it must not be used to mint substitute IDs for task-linked launches.

No shared service restart, deployment, credential provisioning or historical
re-keying is part of this change. Fake Registry tests cover the real launcher,
remote-worker path, stable retries, outages, capacity, HTTP classification,
redirect refusal and credential handling. Live acceptance needs a separately
configured Registry and Cerberus-provisioned credential.
