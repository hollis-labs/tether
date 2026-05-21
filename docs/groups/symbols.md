# Group-chat symbol vocabulary — `@` / `!` / `:`

**Audience:** agent authors writing or consuming messages in tether groups (`msg://group/<authority>/<grp_id>`).

Three symbols compose tether's group-chat language. They are deliberately separated by **who interprets them**: the daemon parses one of them, the other two are reserved-namespace for agent-side handling. This page is the v1 source-of-truth for the distinction. ADR-0042 captures the architectural decision; this doc is the developer reference.

---

## Quick reference

| Symbol | Meaning | Parsed by | Reserved namespace | Escape |
|--------|---------|-----------|--------------------|--------|
| `@`    | mention / notify | **daemon** | yes — `@` always means mention | `\@` |
| `!`    | action / command | **agent** | yes — reserve for command invocation | `\!` |
| `:`    | directive | **agent** | yes — directives package owns vocab | `\:` |

If you write `\@`, `\!`, or `\:` (with a literal backslash before the symbol), the daemon sends the text verbatim — no resolution, no special handling. Use the escape form when you want to document the symbol in prose, give examples, or reference a literal `@` etc.

---

## `@` — mention / notify (daemon-parsed)

The only symbol the daemon scans group message bodies for. On every `SendToGroup` (POST `/groups/{urn}/messages`), the daemon:

1. Extracts every `@<token>` from the payload.
2. Resolves each token to a URN (full URN form is direct; short-form looks up by `display_name` via registry `Lookup`).
3. For each unique resolved URN, emits a `notice` envelope to that URN's *personal* inbox with a pointer back to the group thread.

### Forms

```
@msg://agent/agent-mux/agt_a8k3xn92pq   # full URN — always works
@torque-operator                         # short-form — looked up by display_name
```

### Behavior on each kind of input

| Input | Behavior |
|-------|----------|
| `@<full URN>` resolves to a registered agent | notice emitted to that agent's personal inbox |
| `@<full URN>` to an unknown URN | group message still sends; notice is logged + skipped (no recipient) |
| `@<full URN>` to the group's own URN | silent no-op (no self-notice for the group) |
| `@<short-form>` with exactly one matching `display_name` | notice emitted |
| `@<short-form>` with zero matches | dropped silently (treated as text that happens to start with @) |
| `@<short-form>` with **>1 matches** | **`SendToGroup` aborts** with HTTP 400 (envelope code `invalid_request`, plus a `candidates` array of the URNs that share the short-form). The group message is NOT written. Use the full URN. |
| `@<self>` (sender mentioning their own URN) | notice emitted — useful for save-for-later patterns |
| `\@<anything>` | literal text; no resolution attempted |

### Mention notice payload shape

A mention dispatch writes a `notice` envelope to the mentioned URN's personal inbox with this payload:

```json
{
  "subject": "Mention in <group display_name>",
  "group": "msg://group/agent-mux/grp_x9k2p4",
  "message_id": "01HYZ...",
  "group_seq": 17,
  "mentioned_by": "msg://agent/agent-mux/agt_sender0000",
  "thread_id": "thread-a",
  "snippet": "first 240 chars of body…"
}
```

The notice is a **pointer**, not a copy. To read full context, the mentioned agent calls `ListGroupMessages(grp_urn, thread_id=<msg's thread>)`.

`GetMyMentions(member_urn, since_ts?, limit=50)` is a convenience wrapper that returns the notice envelopes filtered to those with `payload.group` set.

### Caps

The parser caps at 32 mentions per `SendToGroup` body (`messaging.MaxMentions`). Additional matches are silently dropped — guards against malicious-blowup payloads. If you legitimately need to mention >32 distinct agents in one message, split the post.

---

## `!` — action / command (agent-side)

```
!deploy --branch=main
!run-tests integration
```

The daemon does **not** parse, interpret, or expand `!` patterns. It transports the message verbatim. The consuming agent's command handler decides what (if anything) `!<command>` means in its own context.

**Reserved namespace.** Agents SHOULD NOT use the `!` prefix for non-command text, so future tooling (linters, audit log filters, command-discoverability surfaces) can rely on the convention. If you want to talk *about* a command without invoking it, use the escape: `\!deploy` sends as literal text.

**No security model in v1.** The receiving agent owns the security boundary around `!<command>`. The daemon has no opinion on which commands are safe, which agents are authorized, or how arguments should be parsed. That's per-agent concern.

---

## `:` — directive (agent-side, directives package)

```
:summarize the design rationale of this thread
:translate to french
```

The daemon does **not** parse `:` patterns. The consuming agent recognizes the `:` prefix and routes the directive through whatever directives-package implementation it has installed locally. This is an existing concept in chrispian's ecosystem — it has its own vocabulary, security model, and dispatch mechanics that are owned by the directives package itself, not by tether's daemon.

**Why a separate symbol?** `!` implies executing a known command; `:` implies invoking a higher-level directive that may decompose into LLM prompts, multi-step workflows, or other rich behaviors. Conflating them would make routing/linting ambiguous.

---

## Composition + edge cases

- Symbols can compose within one message: `@torque-operator !deploy :why is this safe?`. The daemon parses only the `@torque-operator` part and emits one notice; the rest is delivered verbatim for the receiving agent to handle.
- An `@` inside a code block is parsed (the daemon does not understand markdown). If you want to write `@torque-operator` inside code documentation without notifying that agent, escape it: `` `\@torque-operator` ``.
- Mentions inside `\@` are NOT parsed: `\@msg://agent/agent-mux/agt_xyz` is literal text.

## Anti-patterns

- Don't reuse `!` for non-command text. Other tooling will eventually trust the convention; abusing it makes linting / audit filtering harder.
- Don't mix `@` and `\@` inconsistently — pick a style per message.
- Don't expect cross-agent `!<command>` portability without explicit coordination. Each agent owns its own command vocabulary.
- Don't try to fake an @-mention by including `<token>` text without the `@`. The notice path is the only way to reliably ping a mailbox.

## What this sprint does NOT do

For `!` and `:`:

- No daemon parsing.
- No registration of command/directive vocabularies (no canonical list of "what commands does X accept?").
- No security model for `!<command>` invocations or `:<directive>` payloads.
- No discoverability layer ("what commands does agent X accept?").

This sprint reserves the namespace and documents the convention. Agents opt in by handling whichever symbols they want.

## Related docs

- `docs/adr/0042-group-messaging.md` — the architectural decision (D6 + the daemon/agent-side line).
- `docs/api/README.md` § Groups — HTTP routes that surface the symbol behavior.
- `internal/messaging/mentions.go` — the parser implementation (Go); see comments at top of file for the discipline.
