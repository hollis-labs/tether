- Claude hook mapper for the activity adapters (CW-20261014-0143, slice 2): `agentevents.ClaudeHook` maps a Claude Code hook
  payload to catalog events at the `exact` tier (SessionStart, UserPromptSubmit, Pre/PostToolUse and failure, PermissionRequest/
  Denied, Notification permission/elicitation/idle, SubagentStart/Stop, Stop, SessionEnd). It is pure, records only tool names,
  ids and sizes (never prompt, tool input or result content), ignores events and notification types it does not know, and plants
  nothing yet. Observations may now carry `class.labels`. A subagent stop is the new `agent.subagent_stop` kind (architect ruling agent_adapter_event_kinds_2026_10_16; declared locally until harness/core v0.12.0 registers it); TeammateIdle is left to the adapter that can resolve team membership.
