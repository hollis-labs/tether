# Team roster commands

These commands use the owning Tether daemon and its authenticated caller. The
team module must already be enabled and the caller must have the applicable
authored team permissions. The CLI does not create grants or choose a caller
identity.

```sh
tether team ls
tether team ls RUN_ID
tether team ls --limit 20 --after LAST_RUN_ID
tether team msg RUN_ID workers 'Check the accepted task' --key message-1
tether team msg RUN_ID workers --body-file brief.txt --key message-2
tether team stop RUN_ID MEMBER_ID --key stop-1
tether team stop RUN_ID MEMBER_ID --cascade --key stop-tree-1
tether team stop RUN_ID --all --key end-run-1
tether team boot --definition team.json --key boot-1
```

Output is JSON. `ls` returns only runs containing the authenticated actor with
matching actor kind and retained membership. Ended membership remains visible.
`next_after`, when present, supplies the next page cursor. Member views contain
member ID, slot, actor, kind and status; session receipts, provision intents and
private host metadata are omitted. Listing neither journals a mutation nor
acknowledges messages. `GET /teams/roster` accepts only `run_id`, `after` and
`limit` (1–100); `run_id` and `after` cannot be combined.

`stop` requires an explicit member or `--all`. It invokes the existing authorized
cancel or dissolve operation, preserving retained targets and their idempotent
stop receipts. `msg` invokes the existing team address operation; a slot or
authored route is resolved by the daemon, and delivery retains the selected
recipient sessions. Reuse a key only for the exact same operation and content.

`boot` sends an authored team JSON definition to the existing form operation.
Definitions still need the accepted revision, definition pins, pool identities,
limits and strict authority policy. Forming a run does not elevate its caller or
turn a person/service governance participant into a provider session. Required
actor slots are provisioned through the existing host admission.

Each actor slot may include non-secret text in its existing `workspace` map:

```json
{
  "workspace": {
    "mission": "Implement the accepted task and preserve its evidence.",
    "brief": "Read the current source before changing this worker's slice."
  }
}
```

This fragment belongs inside an otherwise complete authored slot. The accepted
mission is planted as `MISSION.md`, the brief as `brief.md`, and a context receipt
as `TEAM-CONTEXT.json` in the new boot directory. The provider boot prompt directs
the actor to read those files. Both catalog and spec launch engines retain this
context through the ordinary artifact admission. Existing catalog file
collisions, changed retries and unsupported context ports refuse rather than
silently dropping context. Earlier team plans without context retain their
existing behavior. Text is persisted in the launch plan: do not put credentials
or other secrets here. These keys do not configure cwd, sandbox, provider trust,
MCP servers or actor authority.

The kit cannot yet be retired on the strength of these commands alone. Its
`cleanup` also removes stopped-agent scratch, inactive merged clean worktrees,
and stale provider temporaries. There is no daemon team cleanup command in this
slice. The existing direct-database `workspaces prune` command is not a substitute:
team cleanup needs exact retained resource ownership, proven inactivity and
native-custody fences, and must refuse unknown resources. Active team resources,
native state, pending deliveries, receipts and lifecycle evidence remain retained.
No installed boot configuration or running session is changed by this source.
