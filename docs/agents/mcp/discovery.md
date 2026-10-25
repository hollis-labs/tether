# Choose a discovery mode and call a tool

Flat is the default: the client sees native Tether tools and each connected,
granted upstream tool under its real name. Search exposes four tools:
`tether_tool_search`, `tether_tool_list`, `tether_tool_call`, and
`tether_gateway_status`.

## Pick the client surface

Use flat when client permissions or hooks need to match individual tool names.
Use search to keep the initial tool schema inventory small. In search mode,
client permissions and hooks see `tether_tool_call`, so they cannot identify its
downstream target by the client-visible tool name alone. That dispatcher can
perform destructive operations and advertises that fact.

```sh
tether mcp --proxy --discovery-mode flat
tether mcp --proxy --discovery-mode search
```

An environment selector is also supported:

```sh
TETHER_MCP_DISCOVERY_MODE=search tether mcp --proxy
```

Or set this fragment in the catalog's `global.yaml`:

```yaml
mcp:
  discovery_mode: search
```

Selection is CLI flag, then environment, then gateway config, then persisted
Tether setting, then `flat`. Profiles have an intervening mode hook but profile
selection is planned in CW-20260926-0008. Explicit empty or unknown modes are
errors, including invalid values in a lower tier that another tier overrides.
Mode resolves at startup; restart the client endpoint to apply a change.
`--servers` restricts reachability in both modes, as described in
[Limit tools](limit-tools.md).

## Search, hydrate, call

The following blocks are tool arguments, not shell commands. In search mode,
call `tether_tool_search`:

```json
{"query":"tether_session_list"}
```

Read `items` for exact tool names and origins. Results are ranked, so the order
carries meaning; `origin_counts` summarizes how many came from each origin. Pick
how much each item carries with `detail`:

| `detail` | Each item carries | Use it to |
|---|---|---|
| `names` | name and origin | scan cheaply |
| `summary` (default) | name, origin, title, a one-line description (the first sentence, at most 160 characters), annotations and `required_args` | choose a tool, and often call it without hydrating |
| `schema` | the full description, input and output schemas | construct a call in one step |

`required_args` lists the top-level required argument names (at most 12), in the
schema's order; it is not a schema, so an argument's type still comes from
`tether_tool_list`. For a known name, call `tether_tool_list` to get its real
schema:

```json
{"names":["tether_session_list"]}
```

A response is capped at 32 KiB of items and is cut at a whole tool, never in the
middle of one. When that happens `truncated` is true and `truncation_reason` is
`"bytes"` (a page of the requested size would not fit; `"limit"` means the page
was full). For enumeration and search, `next_cursor` resumes exactly where the cut
fell. Hydrating by `names` has no cursor, so the names that did not fit come back
in `omitted_names`: request them again. The first tool is always returned, however
large, so a caller can make progress.

Then call `tether_tool_call`, using the schema to construct `arguments`:

```json
{"name":"tether_session_list","arguments":{}}
```

In flat mode, call `tether_session_list` directly with `{}` instead.
For paginated discovery, `next_cursor` continues the same query; `truncated`
means more matches exist. `unavailable_servers` lists every app that cannot serve calls
right now, with the reason in `unavailable_reasons`; the matches of those apps are not in
the results, so a tool that is missing may belong to one of them. Two kinds, and only one
of them flips `complete`:

- **unavailable** (`failed` after the restart limit, or `excluded`): known down or switched
  off. Reported once, with its reason. `complete` stays true, because nothing more is coming
  until someone fixes it; a permanent gap that flipped the flag would teach callers to
  ignore it.
- **incomplete** (`starting`, `reconnecting`, or a state not recognised): should be present
  and is not yet. `complete` is false and the app is in `incomplete_servers`; retry shortly.

`tether_gateway_status` carries the same three fields.
Use [Troubleshoot](troubleshoot.md) for connection and visibility failures.

## Measuring discovery

`make search-eval` scores tool discovery against a versioned fixture and prints
recall@1, recall@5, recall@10, MRR and median response bytes per scorer, overall
and for the head (most-used tools), tail (rarely used) and hard (near-duplicate,
synonym, cross-origin, typo) query groups. It fails if the numbers differ from the
committed `baseline_<scorer>.json`, so a scorer change has to show its effect.

The fixture lives in `internal/mcpgateway/testdata/searcheval/`:

- `catalog.json` is a pinned snapshot of the 725-tool operator inventory (names,
  origins, descriptions, annotations, required argument names; no schema bodies
  and no argument values), with a sha256 over its entries. Expectations are only
  meaningful against a fixed catalog, so editing it by hand fails the digest check.
- `queries.json` holds the labeled queries: intent-only text, the tools that
  satisfy it (any one counts), a category, and `review: true` where the labeler
  was unsure. The first set is AI-labeled and says so; it has not been human
  reviewed, so treat small differences between scorers on `review` queries with
  care. A label whose tool is missing from the snapshot fails loudly as `STALE LABEL`.
- `baseline_keyword.json` is the committed result of the production scorer, with
  the rank of the first expected tool for every query so a change shows what moved.

To evaluate another scorer (BM25, the code-mode experiment), add it to
`searchEvalScorers` in `searcheval_test.go`, record its baseline with
`-args -search-eval-update`, and compare the two tables.

To refresh the snapshot after the catalog changes, dump the live inventory and run
`go test ./internal/mcpgateway -run TestSearchEvalSnapshot -args -search-eval-snapshot-from=<dump.json>`
(set `SEARCH_EVAL_CAPTURED` and `SEARCH_EVAL_SOURCE`), then relabel anything the
fixture test flags and re-record the baselines. `SEARCH_EVAL_LIVE_INVENTORY=<dump.json>`
makes `TestSearchEvalLiveDrift` report labels whose tools have left the live gateway.
The real discovery history is small and biased toward a few tools, so the head
queries mirror usage and the tail and hard queries exist to keep the eval honest.

## Search scorer

`tether_tool_search` ranks with BM25 (the shared `toolselect` ranker from
`libs/agent/core`): inverse document frequency over each tool's name, title,
description and origin tags, with an exact-name tier and a name-prefix tier ahead
of it. The original word-count scorer is still available:
set `TETHER_MCP_SEARCH_SCORER=keyword` in the daemon's environment and restart to
switch back; any other value, or unset, is BM25.

On the search-eval fixture (138 queries, 725 tools) BM25 moved recall@5 from 0.594
to 0.804 and MRR from 0.456 to 0.690, with the median response size and per-search
latency unchanged (`go test ./internal/mcpgateway -bench SearchScorers`: about
8 ms per call for both, dominated by the cursor fingerprint). The fixture does not
include input-schema argument names, so the numbers compare the two scorers on the
same fields (name, title, description, tags). BM25 does not stem, so "messages"
does not match "message"; the typo queries in the fixture still miss.
