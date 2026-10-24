# Diagnose missing MCP tools

Start with `tether_gateway_status`, available in flat and search modes. It
reports the effective mode/source, origin connection states, cataloged,
eligible and available target counts, and whether the inventory is complete.
Counts describe targets; they need not equal the number of gateway wrapper
tools a search-mode client sees.

## Check one tool

Call `tether_gateway_status` with:

```json
{"name":"tether_session_list"}
```

Read `visible` and `reason`. In search mode, a target can be connected and
eligible while `visible: false` means it is not a directly advertised client
tool. Hydrate it with `tether_tool_list` and dispatch it through
`tether_tool_call`; see [Discovery](discovery.md#search-hydrate-call).

## Match the failure to the next action

| Symptom | Check |
|---|---|
| Startup rejects an unknown/disabled server ID | Compare the selected IDs with enabled catalog entries; [Limit tools](limit-tools.md) explains selectors. |
| Wrong discovery surface | Read status `mode`/`source`; explicit CLI/env/config values determine it, not client brand. |
| Origin excluded with “cannot be confined locally” | Read [Protection](protection.md#remote-upstreams-require-an-explicit-waiver); remote servers require an operator decision. |
| Origin failed, restarting or exhausted | Read its error and recovery fields; correct the upstream executable, credential or endpoint, then restart the client proxy. |
| Search found nothing and `complete` is false | Inspect unavailable origins; an incomplete inventory does not establish absence. |
| A mutation fails auth/scope checks | Check the proxy's token and scope configuration, separately from upstream credentials. |
| Daemon-only startup fails | Restore the daemon connection; that proxy intentionally has no database fallback. |

## Check catalog and filesystem configuration

```sh
tether doctor --json
```

Doctor validates discovery configuration, enabled file credential references,
daemon reachability and reported protection posture. Its credential-file check
does not call helpers or print credential contents. For missing upstream registration, use
[Add an upstream](add-upstream.md); for AI-budget errors, use
[Budgets](budgets.md#ai-cost-budgets).

## A call failed with a wrong or missing argument

When a tool call fails and the arguments you sent do not fit that tool's own
schema, the gateway adds a second content block after the upstream's error. It
is a JSON object under `gateway_hint`:

```json
{"gateway_hint":{"tool":"torque_comment_add",
  "unknown_args":["body","task_id"],
  "did_you_mean":{"body":"content","task_id":"entity_id"},
  "missing_required":["entity_type","entity_id","content"],
  "required_args":["entity_type","entity_id","content"],
  "example":{"entity_type":"<entity_type>","entity_id":"<entity_id>","content":"<content>"}}}
```

| Field | Meaning |
|---|---|
| `unknown_args` | names you sent that the tool does not declare |
| `did_you_mean` | your name → the declared name it most likely meant (a suggestion, not a guarantee) |
| `missing_required` | declared-required names you did not send |
| `required_args` | all required names |
| `accepted_args` | every declared name, only when there are 15 or fewer |
| `example` | a minimal call built from the required arguments, with placeholder values: the shape, not real data |
| `next` | for a tool with many arguments, the `tether_tool_list` call that returns the full schema |

Rules worth knowing:

- The hint is derived only from the tool's advertised schema and the arguments you
  sent, never from the upstream's error text, so it reads the same for every app.
- It is attached only to a call that **already failed**. A successful call is never
  modified, even if it carried a name the tool ignored; that silent-drop problem is
  handled separately (see the argument-integrity standard).
- Names starting with `_` (transport metadata) are not reported as unknown, and a
  schema that sets `additionalProperties` to true or to a schema accepts extra names.
- `tether_tool_search` with `detail: schema` and `tether_tool_list` return an
  `example` on each item, taken from the schema's own `examples` when it has one.

Related behaviors on the Tether-owned message tools:

- `kind` is a real enum (`request`, `response`, `notice`, `status_update`,
  `handoff`, `escalation`); a wrong value is refused with the valid ones named.
- A `payload_json` string that is not valid JSON is refused as `invalid_request`
  with `field: payload_json` and the parse error. A JSON object is accepted and
  serialized.
- `tether_message_reply` answers only a message a session published to a channel.
  For ordinary mail it fails with `reply_target_not_a_session`, and the error now
  carries `helpTool: tether_message_send` and a `nextStep` saying to send to the
  original sender with `in_reply_to` set.
