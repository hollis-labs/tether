package wakeintent

import (
	"fmt"

	"github.com/hollis-labs/libs/mesh/core/messaging"

	"github.com/hollis-labs/tether/internal/messaging/envelope"
)

// WakeGuidance is the instruction every generated mailbox wake carries. The wake is a pointer
// to the mail, not a turn to report on: a mail check with nothing actionable ends silently
// (CW-20261114-0009). A decision or approval Chrispian must give goes out as its own typed
// message, which Tachyon's Needs-you shows, never as prose in a reply (AskGuidance,
// CW-20261114-0127).
const WakeGuidance = "Read it from your inbox. If nothing needs Chrispian, mark it read and end the turn silently, with no message. Otherwise handle it; it does not interrupt your current task unless it is urgent. Marking a message read or consumed settles it; this is the only push you get for it. " + AskGuidance

// AskGuidance tells a woken seat how to ask Chrispian for a decision or approval.
const AskGuidance = "If you need a decision or approval from Chrispian, send it as its own typed message (tether_message_post to @chrispian, kind=question, decision or approval_request, attention=human; the `ask` skill), never inside your reply."

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
