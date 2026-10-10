# Remote session paths and logs

An explicit `--target tcp:localhost:PORT` selects a worker reached through an
SSH local forward. It bypasses the local catalog, state database and workspace
files. Supply an already enrolled device credential with `--token-file` (a
client-owned 0600 file) or `TETHER_TOKEN`. The CLI does not discover an operator
token or enroll/issue credentials for this target. An empty or invalid target
is an error. Keep passing the target and credential options on subsequent
commands, including attach commands suggested by launch output.

```sh
tether --target tcp:localhost:18090 --token-file /private/client/worker-device.token sessions list
tether --target tcp:localhost:18090 --token-file /private/client/worker-device.token sessions get SESSION_ID
tether --target tcp:localhost:18090 --token-file /private/client/worker-device.token sessions tail SESSION_ID --follow=false
```

The initial remote CLI admission list covers `sessions list`, `get`, `stop`,
`tail`, `attach`, `input`, `turn`, and ordinary `launch`. The daemon still
enforces its existing independent device scopes and launch policy. Launch
file/inline override, injection, and Torque augmentation flags are refused by
this CLI until their remote input contract is supported. Other commands refuse
before running their handlers, including workspace pruning, catalog discovery,
boot generation/execution, diagnostics, MCP/ACP, and service operations. This
includes commands with an API equivalent that has not yet been connected to
this target selector. Use the authenticated API for those supported remote
routes. No error path silently reverts to client-local catalog/state/files.

Without `--target`, the local CLI retains its existing socket/config selection,
operator credential behavior and offline fallback. Local snapshot tail still
reads the complete local session log.

## Response references

On the remote listener, session list/get `workspace` and create/start/resume
`workspace`/`log` values are opaque `tether-ref:workspace:...` and
`tether-ref:log:...` references. The suffix is a digest of resource kind and
session identity, never an encoded path. A reference is scoped to the selected
environment; it is neither globally addressable nor an authorization token.
Use the session ID and authenticated API to retrieve a resource. Do not pass a
reference to file operations or interpret it as a client path. Replay replies
use the same projection. Local socket responses retain the original paths.

Catalog records can describe daemon-side repository roots, command definitions
and workspace specifications. Filesystem diagnostic paths and daemon-log
diagnostics likewise describe the worker host. They are not client filesystem
inputs. Sysop remains local-only. General event payload redaction and catalog
schema redesign are outside this change.

## Bounded session log API

`GET /sessions/{id}/log` requires **terminal** on the remote listener because
the persisted log can include raw PTY output and process stderr. `read` remains
the scope for structured session state/events. `admin` does not imply terminal.
Filesystem helpers and `/logs/daemon` continue to require **maintain**.

The daemon resolves its stored session workspace and opens only
`logs/session.log` within that workspace using a confined filesystem root.
There is no client-supplied path parameter; symlink escapes and non-regular
files are refused. Missing logs return 404. File/access/read error envelopes
omit underlying host paths.

Query parameters:

| Parameter | Meaning |
| --- | --- |
| `limit` | 1–65536 bytes; default 65536. |
| `offset` | Non-negative byte offset; omit for the last `limit` bytes. |
| `generation` | Opaque value from a prior response; requires `offset`. |

A successful response contains `data` (base64 bytes), `offset`, `next_offset`,
`size`, and `generation`. Byte encoding preserves non-UTF8 output. To continue,
send `offset=next_offset&generation=GENERATION`, with a desired bounded limit.
Append preserves generation. A changed generation or an offset past the
current size returns 409 `log_changed`; start a fresh snapshot instead of
mixing ranges. This cursor belongs only to the session log, and cannot be
compared with environment/channel event sequences or raw attach PTY cursors.

Generation state is a bounded in-memory cache (512 entries). Replacement,
observed truncation, or prefix rewrite invalidates it; daemon restart/cache
eviction can also invalidate continuation safely. A same-inode truncate and
regrow that occurs entirely between requests and restores an identical leading
4096 bytes is indistinguishable from append. This endpoint is a bounded log
reader, not a durable file mutation journal. Callers requiring durable output
continuity should use the environment event stream and its own gap protocol.

Remote `sessions tail --follow=false` emits a single bounded tail snapshot.
Follow/attach uses the existing authenticated stream. A stopped-session tail
fallback uses this API. Transport/auth/range errors return to the caller;
cancellation propagates and does not trigger an offline file read.
