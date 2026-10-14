# Local operator sessions as mesh actors

A Claude/Codex/AGY session the operator started outside Tether has no Tether
session ID, so agents cannot find it with `tether_session_get`. It can still be
a messaging actor using existing pieces; no new daemon feature is required.

1. Register a profile once (kind `agent`) with `tether registry register
   --kind agent --file operator.yaml --print-urn-only`. The URN is the stable
   identity; the messaging address is `msg://agent/<substrate>/<name>` from the
   profile.
2. Poll its mailbox from the local session with the hook in
   [external-session-wake.md](external-session-wake.md) (`tether messages
   claude-hook --as <address>`). Mail queues in the durable inbox until read.
3. When delivering work, pass the reply address so the receiving agent knows
   where to answer:

   ```
   tether sessions turn <id> "please review X" --reply-to msg://agent/local/lead
   ```

   The flag prepends a `[reply-to: ...]` line to the turn text (client-side
   framing only; the turn API is unchanged).

## Interim convention

Until an operator session is registered, agents answer with a Torque comment on
the task named in the request. Use `--reply-to torque:<task-id>` to make that
explicit. Manager/AOM role charters (kept outside this repo) should state this.
