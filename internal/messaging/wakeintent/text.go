package wakeintent

import (
	"fmt"

	"github.com/hollis-labs/libs/mesh/core/messaging"
)

// WakeGuidance is the instruction every generated mailbox wake carries. The wake is a pointer
// to the mail, not a turn to report on: a mail check with nothing actionable ends silently
// (CW-20261114-0009).
const WakeGuidance = "Read it from your inbox. If nothing needs Chrispian, mark it read and end the turn silently, with no message. Otherwise handle it; it does not interrupt your current task unless it is urgent. Marking a message read or consumed settles it; this is the only push you get for it."

// DefaultWakeText is the generated mailbox-wake turn, used when the sender supplied no wake
// text. unread < 1 omits the count (the sweep's retried wakes do not recompute it). retried
// marks a wake re-sent by the sweep.
func DefaultWakeText(env messaging.Envelope, unread int, urgency string, retried bool) string {
	label := "daemon-injected"
	if retried {
		label = "daemon-injected, retried"
	}
	count := "unread mail"
	if unread > 0 {
		count = fmt.Sprintf("%d unread message(s)", unread)
	}
	return fmt.Sprintf("**Mailbox wake (%s)** — %s. Latest: `%s` from `%s` to `%s`, kind `%s`, urgency `%s`.\n\n%s",
		label, count, env.ID, env.From.URN(), env.To.URN(), env.Kind, urgency, WakeGuidance)
}
