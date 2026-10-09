# Message polling outside Tether

CW-20261008-0125 adds `tether messages claude-hook` for Claude sessions launched
directly by a user, including a terminal or tmux session. The same command can
run inside Tether when useful. It polls the configured mailbox through the
existing daemon client and emits a count plus a fixed message-check reminder.

## Official interface survey

Surveyed 2026-10-09. These are documented capabilities, not a claim that every
installed CLI version exposes them. No CLI session or installed configuration
was changed for this spike.

| CLI | Prompt/turn context and continuation | Timer and idle wake | Counts and packaging |
| --- | --- | --- | --- |
| Claude Code | `SessionStart` and `UserPromptSubmit` accept `additionalContext`; `Stop` can continue with `decision: block` and a reason. Check `stop_hook_active`. | Command `asyncRewake` can wake idle Claude on exit 2. Ordinary async output waits for the next turn. `/loop` schedules prompts in a running session, firing when idle. | Hooks can surface a computed count using `systemMessage`. Settings and plugins package hooks. |
| Codex | `SessionStart` and `UserPromptSubmit` accept developer context. Synchronous `Stop` can request a continuation. | Background hooks do **not** start an idle turn. The CLI has no Scheduled management interface; a same-session periodic injection contract was not established by these docs. | Hook context/`systemMessage` can carry counts. Hooks load from config layers and plugins, subject to Codex's hook trust controls. |
| Antigravity | `PreInvocation` can inject an `ephemeralMessage` or `userMessage`; `Stop` can return `decision: continue` with a reason. This is invocation-level, not a documented Claude-shaped prompt-submit event. | `/schedule` documents timers/cron for CLI and 2.0. The 2.0 sidecar scheduler plus `agentapi send-message` documents an existing-conversation injection path. | Injected messages can include computed counts; hooks/plugins provide packaging. The sidecar path is documented for 2.0, not established here for every CLI/IDE installation. |

Sources: [Claude hooks](https://code.claude.com/docs/en/hooks),
[Claude scheduling](https://code.claude.com/docs/en/scheduled-tasks),
[Codex hooks](https://developers.openai.com/codex/hooks),
[OpenAI Scheduled interface](https://learn.chatgpt.com/docs/automations),
[Antigravity hooks](https://antigravity.google/docs/hooks),
[Antigravity slash commands](https://antigravity.google/docs/slash-commands/),
[Antigravity sidecars](https://antigravity.google/docs/sidecars).

Use the documented extension interfaces first. A CLI modification would need a
maintained provider-specific implementation; no undocumented “near-full
control” or transcript-editing interface is assumed. Codex and Antigravity
adapters remain future implementations; the command in this change emits the
Claude contract only.

## Design and contract

Use lifecycle polling for a small, automatic active-session path, then a native
session scheduler for recurring idle checks. The optional bounded Claude
background wait provides a one-shot idle notification without launching an
external service or creating another model session.

`--as` explicitly selects a mailbox. The normal CLI credential selection and
daemon authorization remain authoritative; a hook's external `session_id` is
not a canonical Tether identity. Configure the existing credential for the
intended principal and mailbox read scope. A path supplied through the existing
`--token-file` option may be used; never put bearer contents in a hook command
or example. The helper does not mint credentials, register actors, add tools,
fall back to SQLite, or retry with a different identity. Hook JSON cannot select
a catalog, token, or recipient. Prompts and transcripts are not inspected.

Polling calls `GET /messages/list` with `as=to`, `unread_only=true`, archived
messages excluded, and `limit=1`. The count uses the response's total, not the
page length. “Unread” is the existing `read_at IS NULL` filter; it is not a new
delivery or eligibility definition. No message body, subject, sender, ID, raw
API error, or credential is copied into hook feedback. The existing endpoint
returns a bounded envelope page to the client, which the helper never renders.

The helper never marks delivered, read, consumed, or archived and never claims
or acknowledges a delivery. A reminder is advisory and may repeat until the
agent handles the mail and explicitly marks it read using its existing
authorized interface. Handling and delivery acknowledgement stay with that
interface. This does not change hosted wake ownership or private provider
custody.

Normal polling emits one JSON object: `{}` for zero unread, context plus a user
count for start/prompt events, or a fixed Stop continuation reason for unread
mail. A Stop invocation with `stop_hook_active: true` emits `{}` without polling,
preventing a repeat continuation from this hook. Stop input must include that
boolean. Malformed/oversized JSON, unsupported events, daemon errors and denials
fail without an unread notification. Hook input is capped at 1 MiB; per-request
timeout defaults to two seconds and is bounded to ten seconds.

## Opt-in configuration

Merge [the example](../examples/hooks/claude-message-polling.json) into the
appropriate Claude settings file only when authorized to change that file.
Replace its example mailbox and command executable with the intended identity
and installed binary. Preserve existing hooks. The example polls on startup,
resume, prompt submission and Stop; it does not poll a session that remains
idle indefinitely.

For a documented, recurring idle cadence, a user can request this in a Claude
session that supports `/loop`:

```text
/loop 1m Check my configured Tether mailbox using its existing authorized message interface; handle unread mail within current permissions and mark read after handling.
```

Claude's scheduler requires a running session and does not provide a daemon
delivery receipt. This is a recipe, not an automatically installed schedule.

For a **one-shot**, five-minute idle notification, add this command handler to
a `SessionStart` matcher for `startup|resume` on a Claude version supporting
`asyncRewake`:

```json
{
  "type": "command",
  "command": "tether messages claude-hook --as msg://agent/example/worker --wait 5m --interval 5s --timeout 2s",
  "asyncRewake": true,
  "timeout": 310
}
```

`--wait` is accepted only for `SessionStart`, is bounded to ten minutes, polls
immediately and then at the configured interval, and exits after the first
confirmed unread result. Only that result returns exit 2 with a fixed reminder
on stderr, which Claude's documented `asyncRewake` interface uses to wake the
session. Empty expiry exits 0 without output. Failure exits 1, never 2. This is
not a persistent watcher, and simultaneous opt-in hooks can produce duplicate
advisory reminders. There is no local deduplication receipt or automatic
rearming. Prefer `/loop` when a recurring cadence is wanted. Do not place this
wait handler on synchronous prompt hooks.

## Verification boundary

Focused tests use private fixture mail and servers to exercise the real list
endpoint/client, explicit credential forwarding, recipient query, unread count,
archive exclusion, unchanged delivery/read state, sanitized output, the Stop
guard, denial, deadlines, cancellation and delayed-mail waiting. They do not
launch Claude, Codex or Antigravity, and do not establish installed hooks,
tool availability, or live provider wake acceptance.
