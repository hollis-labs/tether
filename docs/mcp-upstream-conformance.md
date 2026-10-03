# MCP upstream author conformance

Tether reports ADR 0047 declaration findings in `tether_gateway_status.lint`
and `tether doctor --mcp-live`. Each finding identifies `origin`, the final tool
`name` when applicable, a stable `code`, and a remediation message. Initialization
instructions are origin-level findings with an empty name. Status respects the
endpoint's granted origins and profile; it does not disclose excluded tools.

`doctor --mcp-live` is an explicit operator probe: it resolves configured
credentials and **spawns upstreams**, then performs initialization and tools/list,
with bounded deadlines, confinement rules and redacted output. It never calls
a tool. Default doctor stays offline and checks catalog declarations; it cannot
certify an upstream's live metadata. Failed initialization is unexamined inventory,
not an observed empty tool set. Use `tether doctor --mcp-live --json` to collect
a report and route findings to the owning upstream project.

| Authored rule | Finding code | Author action |
|---|---|---|
| Final names use lowercase `[a-z0-9_]` | `charset` | Rename upstream tools; do not rely on client normalization. |
| Final names identify their origin | `origin_prefix` | Declare the origin prefix in the upstream name or an exact catalog `tool_prefix`. |
| Final names fit 128 Unicode characters | `length` | Shorten the name. |
| Client-qualified names may face shorter limits | `client_qualified_length` | Consider the conservative 63-byte warning, including `mcp__tether__`. |
| Tool behavior is explicitly assessed | `missing_annotations` | Supply truthful annotations; omission remains unassessed. |
| Tool descriptions fit 2,048 Unicode characters | `description_length` | Shorten inline guidance and link fuller documentation. |
| Listed tools match their advertised availability | `disabled_description` | Reconcile a description beginning with Disabled; Tether does not guess availability from prose. |
| Initialization instructions fit 2,048 Unicode characters | `instructions_length` | Shorten the upstream's observed initialize instructions. |
| Input schema is an object JSON Schema | `input_schema` | Fix syntax, keyword constraints, regular expressions or unresolved local references. |
| External schema references are unexamined | `input_schema_unexamined` | Bundle referenced schemas under local `$defs`/`definitions` for checking without network access. |
| Aggregate checking is incomplete | `conformance_unexamined` | Inspect the reported remainder; request another status pass or narrow the live-probe selection. |
| Schema checking has bounded resources | `input_schema_limit` | Simplify the schema; it exceeds the checker limits and is unexamined. |

Each status pass examines at most 128 new declarations and 512 KiB of
conservatively estimated schema work. Findings are memoized once per accepted
definition outside registry/pool locks; repeated reads reuse them. Remaining
named declarations carry `conformance_unexamined`, and status exposes their
profile-filtered `unexamined_tools` count (also including external/oversized
schemas). Later passes can examine the next bounded batch; a large accepted
inventory is never silently called fully checked. Live doctor groups remaining
aggregate-budget findings by origin and reports their count. A fresh doctor
probe has a fresh cache; narrow the operator's inherited `TETHER_MCP_SERVERS`
selection when examining a large upstream portfolio. Collision-only pool status
and startup checks never invoke metadata/schema lint.

Metadata lint is **report-only**: findings never block startup, rewrite schemas,
infer safety or remove tools. Title, annotations, schemas and other supported
metadata remain authored values. Annotation hints are advisory; they do not grant
permission. Duplicate final names are a separate registration error, including
collisions with native/gateway names. Declare `tool_prefix` on one upstream to
separate owners; duplicate names within one origin must be fixed by that author.
See [gateway naming and profiles](mcp.md) for collision/refresh behavior.

Input-schema lint checks a copy with the same JSON Schema parser/resolver used by
the Go MCP SDK, supplemented with basic keyword constraints. The root must declare
`type: object`. Local reference cycles are supported; references are resolved,
not recursively expanded into invented arguments. The checker does not validate
tool-call arguments, defaults, outputs, custom vocabularies or every schema dialect;
a lint-free declaration is not a conformance certification. Remote schemas are
never fetched. Encoding, decoding and resolution are bounded by preflight limits
of 256 KiB encoded size, depth 64 and 4,096 traversed values; a second preflight
checks the decoded SDK schema. These conservative traversal limits can also flag
large typed schemas. Findings never quote raw schemas or instructions. Only the
observed initialize instruction length is retained, and a later initialization
replaces it. Profile instructions retain their separate hard config limit.

Tangent's clean-break tool rename and consumer updates belong to
CW-20261001-0646.
Envelope type IDs are a separate namespace and are not MCP tool names. Tether's
CW-20260926-0011 supplies checking/reporting and this guidance, not changes to
other upstream repositories or live service deployments.
