# Team activation evidence

`GET /teams/activation?launch_id=NAME[&session_id=ID]` (or `?session_id=ID`) returns provider-owned,
report-only observations. General structural facts use the existing `tether.read` scope; responses include `Cache-Control: no-store`. A team session selector must join the retained
session and enrollment receipts; a caller cannot attach an arbitrary pin to a
session by supplying a launch name.

`mapping: legacy_catalog` means the definition pin verifies agentdef-v2 content;
the catalog launch determines execution. The definition's behavior and harness
profile are not consumed by this adapter. `definition_executed` stays false.
`runtime_observed` means a successful provider start was recorded, not that the
process remains alive now. Worker facts are hub assertions, never remote proof.

The response separates the loaded policy pin, current source verification,
installed-policy comparison, persisted current loaded baseline, separately retained admitted-policy baseline, enrollment admission,
sealed created plan and successful launch-time observation. Historical facts remain available with `current_unavailable` when the catalog launch is deleted or invalid. Observations carry recorded/observed times and a process-local catalog reload counter (explicitly unknown when no exact snapshot is available). Missing historical
facts remain absent/unknown. The first loaded baseline is persisted per approved
policy binding, including across daemon restart. A changed launch with the same
pin reports `launch_changed_since_load`. Restart does not overwrite that baseline.
Launch-time sandbox differences report `sandbox_changed_between_create_and_launch`.

`tether.launch-evidence/v1` hashes a normalized selector projection of the actual
persisted plan and launch-time sandbox facts: provider/runtime, catalog model,
permission mode, sandbox structural summary/digest, MCP server IDs, placement/project, route, lifecycle and environment key names. Observed grants, enforcement outcomes and placement availability remain separate fields; they do not alter the comparable plan fingerprint. Known private effective sandbox digests are compared internally at each lifecycle boundary. Missing historical facts remain unknown rather than mismatches. It excludes environment values, argument values, instructions, injected
contents and workspace paths. General readers never receive path-inclusive policy/file digests, including through nested fingerprints or the aggregate digest. The existing local operator check additionally permits `effective_sandbox_policy_digest`, `installed_sandbox_file_digest`, and `installed_sandbox_policy_digest`. Effective policy is captured from the parsed object passed at launch; installed raw bytes are read separately and never substituted for active policy. Canonicalization sorts/cleans selectors without accessing those paths. Paths are never returned.

Sandbox enforcement is a separate closed-state observation from the runtime's sandbox outcome callback: `enforced`, `downgraded`, `not_enforced`, `unsupported_platform`, or `unknown`. Adapters and remote launch acknowledgements without such an observation remain `unknown`; a policy digest or successful process start proves no enforcement. This is a selector
fingerprint, not a hash or attestation of all executable content. Catalog model is
a declaration, not proof of which model a provider ultimately chose. Permission
or sandbox outcomes are never changed by this reporting path.

To propose one new revision without rewriting the whole policy, send
`POST /catalog/plans/team-activation`:

```json
{"name":"review-agent-pin","launch_id":"worker-launch","expected_pin":{"id":"worker","revision":"r1","digest":"sha256:OLD"},"definition":{"id":"worker","revision":"r2","digest":"sha256:NEW"},"source":"catalog:team-definitions/worker-r2.md"}
```

First install the definition artifact through the existing approved catalog
bundle path. The targeted proposal verifies that artifact's pin, compares the
existing pin, preserves other policy entries/fields, and creates a normal catalog
review plan. It makes no live policy change. Existing operator approval by digest,
apply concurrency checks and separate restart requirements still apply. Policy
binding changes and edits to mapped launch files are marked privileged. There is
no partial live activation, new credential, permission redesign or client policy
rewriter.
