package wakeintent

import (
	"fmt"

	"github.com/hollis-labs/libs/mesh/core/messaging"

	"github.com/hollis-labs/tether/internal/messaging/envelope"
)

// WakeGuidance is the instruction every generated mailbox wake carries
// (CW-20261117-0002; it replaced CW-20261114-0009's "end the turn silently"
// default, which left replies unanswered). A wake that needs nothing ends
// silently; anything that asked something gets a reply.
const WakeGuidance = "Read and handle these. Reply to anything that asked you something. End silently only if nothing needs action. Marking a message read or consumed settles it."

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
	behalf := ""
	if who, drafted := envelope.Behalf(env.ContentType, env.Payload); who != "" {
		// An assistant's mail for someone is never that someone's instruction (CW-20261114-0116).
		behalf = fmt.Sprintf(" Sent on behalf of `%s` (drafted by %s): it is the sender's, not %s's own instruction.", who, drafted, who)
	}
	return fmt.Sprintf("**Mailbox wake (%s)** — %s. Latest: `%s` from `%s` to `%s`, kind `%s`, urgency `%s`.%s\n\n%s",
		label, count, env.ID, env.From.URN(), env.To.URN(), env.Kind, urgency, behalf, WakeGuidance)
}
