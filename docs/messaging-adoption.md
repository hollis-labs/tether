# Adopting Tether Messaging

How an app, an agent, or a session you are running by hand joins Tether's
messaging system and starts sending and receiving mail.

This is the task-shaped companion to [messaging.md](./messaging.md), which is
the reference for the surface itself. Once you are past orientation and
actually building,
[messaging-integration.md](./messaging-integration.md) carries the
implementer's decisions — where identity is minted and stored, binding
lifecycle across a restart, and which errors are permanent. If you are adopting on behalf of a whole
application, also read
[`planning/docs/messaging-vnext/T12-cutover-guide-and-adoption-handoff.md`](../planning/docs/messaging-vnext/T12-cutover-guide-and-adoption-handoff.md)
— it carries the capability-by-capability map, the troubleshooting walkthrough,
and the rollout/rollback story.

---

## The three things you need

Tether separates identity from delivery, which is the part that most often
surprises a first-time adopter. Three distinct objects:

| Object | What it is | Lifetime |
|---|---|---|
| **Durable actor** | Who you *are*. A registry profile with a minted URN like `msg://agent/agent-mux/agt_x9k2p4qrst`. | Forever — survives restarts, reinstalls, provider changes. |
| **Canonical session** | One run of a process acting as that actor. | One session. |
| **Runtime binding** | A lease saying "this session currently receives mail for that actor." | Until it expires or is revoked. |

Mail is addressed to the **actor**. The **binding** decides which live session
actually gets woken. An actor with no binding still accumulates mail perfectly
well — it just has nowhere to be delivered right now, and the next session to
lease a binding picks it up.

**You do not need all three to start.** Registering an actor and reading its
inbox by polling works with no binding at all. Bindings matter when you want
Tether to *push* — to wake a live session on arrival.

---

## Rule zero: every read asserts who is asking

Tether's daemon requires every messaging **read** to carry a caller identity as
`?as=<urn>` (ADR 0045, the same-host trust model). This is self-asserted and
unverified — it is an addressing mechanism and an audit trail, **not**
authentication. Same-host callers are trusted; the parameter tells the daemon
which mailbox you mean.

Omit it and the call is rejected with `400 invalid_request`. Every client path
below either attaches it for you or requires you to supply it:

| Path | How `?as=` gets attached |
|---|---|
| `go-tether-client` | `Inbox`/`Subscribe` derive it from the recipient argument; `Get`/`Thread` read it from `WithSelfURN`. |
| CLI | Positional `<to-urn>` argument, or the `--as` flag on `mux whoami`. |
| MCP | A parameter on the tool, but **the name varies**: `mux_message_inbox` and `mux_message_list` take `to`; `mux_message_get`, `mux_message_thread`, `mux_message_consume`, `mux_message_mark_read`, `mux_message_archive`, `mux_message_unarchive`, `tether_group_read` and `tether_group_mentions` take `as`; `tether_whoami` takes `as`. Repair tools (`mux_message_redrive`, `mux_message_purge`) take `authorized_by` instead, recorded as provenance. |
| Raw HTTP | You append it yourself. |

Writes (`send`, `consume`, `cancel`) carry the identity in the body instead.

---

## Path A — a Go application

Pin the client at **v0.2.0 or newer**. Earlier tags predate the `?as=` fix and
every read call will be rejected.

```bash
go get github.com/hollis-labs/go-tether-client@v0.2.0
```

```go
client := tether.MustNew("", tether.WithSelfURN(myURN))
```

At the launch boundary, resolve the session's canonical identity. This is
offline-safe by design — it always returns a usable `SessionID` even when the
daemon is unreachable, and reports the registration outcome separately:

```go
res, err := tether.ResolveSessionBootstrap(ctx, tether.BootstrapOptions{ /* ... */ })
// res.SessionID  — always usable
// res.Registered — whether the daemon accepted it
// res.RegisterErr — why not, if not
```

Then send and read as normal. Durable-delivery primitives (`claim`/`ack`/`nack`)
are **not** wrapped in the typed client yet — use raw HTTP for those
(CW-20260907-0038).

---

## Path B — the `mux` CLI

Register once. `--file` points at a YAML or JSON document matching
`registry.Profile`; `display_name` is the only required field.

```bash
mux registry register --kind agent --file ./my-actor.yaml --print-urn-only
# → msg://agent/agent-mux/agt_x9k2p4qrst
```

Then read and send:

```bash
mux messages inbox msg://agent/agent-mux/agt_x9k2p4qrst
mux messages send --from <urn> --to <urn> --kind notice --body "hello"
mux whoami --as msg://agent/agent-mux/agt_x9k2p4qrst
```

To have a live session woken on arrival, lease a binding. `pull-only` is the
only capability the public lease endpoint accepts:

```bash
mux registry bindings lease \
  --target-urn msg://agent/agent-mux/agt_x9k2p4qrst \
  --session-id sess-1 --host-id host-1 --attempt-id attempt-1 \
  --capabilities pull-only --ttl-seconds 3600
```

---

## Path C — a Claude (or other MCP) session you are running by hand

**Yes, this works today, and it needs no code.** Tether's MCP adapter exposes
the whole messaging surface — 99 tools, including the full registry, bindings,
groups and delivery-trace sets. Point any MCP client at `mux mcp` and an
ordinary interactive session becomes a first-class messaging participant.

### 1. Add the server

```json
{
  "mcpServers": {
    "tether": {
      "command": "/path/to/bin/mux",
      "args": ["mcp"],
      "env": {
        "AGENT_MUX_MCP_TOKEN": "any-opaque-string",
        "AGENT_MUX_MCP_SCOPES": "message.write,registry.write"
      }
    }
  }
}
```

`message.write,registry.write` is the pair a messaging participant needs:
`registry.write` to register an identity and lease a binding, `message.write`
to send and consume. Add `groups.write` only if it will create, administer, or
post to group rooms. **Scopes are per capability group, not a hierarchy** — one
does not imply another. Reads need no token or scope at all, so a read-only
observer can just run `mux mcp` bare.

The token is opaque. Tether checks that one is present, not what it says.

### 2. Register the session as an actor

Call `tether_registry_register` with `kind: "agent"` and a profile object. The
server mints the URN — **do not supply a `urn` field**, that is rejected as
`invalid_request`.

```json
{
  "kind": "agent",
  "profile": {
    "display_name": "Chrispian's Desk Session",
    "role": "operator-session",
    "description": "Interactive Claude session, manually driven."
  }
}
```

Keep the returned URN. That is the address others send to.

### 3. Use it

| To | Call |
|---|---|
| Confirm identity, bindings and group memberships | `tether_whoami` |
| Read mail (non-destructive) | `mux_message_inbox` |
| Mark handled | `mux_message_consume` |
| Send | `mux_message_send` |
| Send and wake a live recipient | `mux_message_notify` |
| Find someone to write to | `tether_registry_search` |
| Diagnose a message that did not arrive | `mux_message_trace` |

`tether_whoami` is the self-discovery call — it answers "who am I, and is
anything currently bound to me?" and tolerates being unregistered rather than
erroring.

### What this does and does not give you

A manually-driven session can register, send, read, consume, join groups and
trace delivery immediately. What it does **not** get for free is *push*: being
woken mid-session by incoming mail requires a runtime binding against a session
the daemon is hosting. A session you launched yourself outside Tether is not
one of those, so treat it as **poll-and-read** — call `mux_message_inbox` when
you want to check. That is a real limitation, not a misconfiguration.

---

## Verifying it worked

```bash
mux registry search --kind agent          # your profile is listed
mux whoami --as <your-urn>                # identity resolves
mux messages inbox <your-urn>             # reads without a 400
```

If a message does not arrive, `mux_message_trace` (or
`GET /messages/{id}/trace`) shows its full delivery state, and §5 of the T12
handoff walks the diagnosis end to end.

---

## Known gaps worth knowing before you build

| Gap | Tracked as |
|---|---|
| No typed `Claim`/`Ack`/`Nack` in `go-tether-client`; raw HTTP only | CW-20260907-0038 |
| Tether cannot act as an A2A *client* — inbound relay only | CW-20260907-0028 |
| `/a2a/` shares a listener with same-host-trust routes; no network boundary | CW-20260907-0029 |
| `registry.VisibilityTetherHosted` is defined but nothing creates it | CW-20260907-0032 |

Reference entries for the identity, binding and group HTTP routes are not yet
in [api/README.md](./api/README.md); [messaging.md](./messaging.md) carries the
surface map in the meantime, and `mux <command> --help` plus `mux mcp` are
always authoritative.
