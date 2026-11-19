package wakeintent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hollis-labs/libs/mesh/core/messaging"

	"github.com/hollis-labs/tether/internal/messaging/envelope"
)

// WakeGuidance is the instruction every generated mailbox wake carries
// (CW-20261117-0002 removed CW-20261114-0009's "end the turn silently"
// default, which left replies unanswered; CW-20261118-0050 made the empty case
// explicit). Anything that asked something gets a reply. When nothing needs
// action the agent ends the turn without writing any message or acknowledgement:
// even a one-line "noted" costs the recipient a turn.
const WakeGuidance = "Read and handle these. Reply to anything that asked you something. If nothing needs action, end the turn without producing any message (no acknowledgement, no \"noted\", no summary): a reply only wakes the sender for another empty turn. Marking a message read or consumed settles it."

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
	return fmt.Sprintf("**Mailbox wake (%s)** — %s. Latest: `%s` from `%s` to `%s`, kind `%s`, urgency `%s`.%s%s\n\n%s",
		label, count, env.ID, env.From.URN(), env.To.URN(), env.Kind, urgency, behalf, typedSummary(env), WakeGuidance)
}

// typedSummary is the typed envelope's gist for the wake turn
// (CW-20261117-0003): who asks what, and for a request what done looks like.
// "" for untyped mail.
func typedSummary(env messaging.Envelope) string {
	if env.ContentType != envelope.ContentType {
		return ""
	}
	var e envelope.Envelope
	if json.Unmarshal(env.Payload, &e) != nil || e.Kind == "" {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n\n%s %s", orDash(e.From), e.Kind)
	if e.Subject != "" {
		fmt.Fprintf(&b, ": %s", e.Subject)
	}
	if e.Re != "" {
		fmt.Fprintf(&b, " (re %s)", e.Re)
	}
	if e.InReplyTo != "" {
		fmt.Fprintf(&b, ", replying to `%s`", e.InReplyTo)
		if e.Status != "" {
			fmt.Fprintf(&b, ", status %s", e.Status)
		}
	}
	if e.Deliverable != "" {
		fmt.Fprintf(&b, "\nDeliverable: %s", e.Deliverable)
	}
	if e.InReplyTo == "" && (envelope.HasOptions(e.Kind) || e.Kind == envelope.KindRequest) {
		b.WriteString("\nAnswer with tether_reply (in_reply_to this id).")
	}
	return b.String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
