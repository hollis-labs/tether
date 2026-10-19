# Team activation evidence

`GET /teams/activation?launch_id=NAME[&session_id=ID]` returns provider-owned,
report-only observations. It uses the existing catalog planning authority and
returns `Cache-Control: no-store`. A team session selector must join the retained
session and enrollment receipts; a caller cannot attach an arbitrary pin to a
session by supplying a launch name.

`mapping: legacy_catalog` means the definition pin verifies agentdef-v2 content;
the catalog launch determines execution. The definition's behavior and harness
profile are not consumed by this adapter. `definition_executed` stays false.
`runtime_observed` means a successful provider start was recorded, not that the
process remains alive now. Worker facts are hub assertions, never remote proof.

The response separates the loaded policy pin, current source verification,
installed-policy comparison, persisted loaded baseline, enrollment admission,
sealed created plan and successful launch-time observation. Missing historical
facts remain absent/unknown. The first loaded baseline is persisted per approved
policy binding, including across daemon restart. A changed launch with the same
pin reports `launch_changed_since_load`. Restart does not overwrite that baseline.
Launch-time sandbox differences report `sandbox_changed_between_create_and_launch`.

`tether.launch-evidence/v1` hashes a normalized selector projection of the actual
persisted plan and launch-time sandbox facts: provider/runtime, catalog model,
permission mode, sandbox policy digest, MCP server IDs, granted session scope
names when available, placement/project, route, lifecycle and environment key
names. It excludes environment values, argument values, instructions, injected
contents and workspace paths. The sandbox digest covers resolved sandbox policy,
including policy path selectors; those paths are not returned. This is a selector
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
