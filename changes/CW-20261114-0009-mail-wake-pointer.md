- The generated mailbox-wake turn is a pointer (CW-20261114-0009): it names the latest message and says "if nothing
  needs Chrispian, mark it read and end the turn silently", instead of asking the agent to run an inbox-check
  procedure and checklist (which produced status chatter). The notify path and the sweep's retried wakes now share
  one formatter, `wakeintent.DefaultWakeText`. A sender-supplied `wake_text` is unchanged.
