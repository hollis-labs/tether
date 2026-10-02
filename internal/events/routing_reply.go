package events

// Reply-to-sender outcomes (CW-20261002-0065, ADR 0049 s1.6). Both are
// session-scoped to the session the reply is currently aimed at, so a
// consumer already following that session sees them.
const (
	// KindRoutingReplyDelivered: the reply body was injected as the target
	// session's next turn. DeliveredToSessionID differs from
	// OriginalSessionID when the originating session had ended and the reply
	// was handed to the session its actor is now bound to.
	KindRoutingReplyDelivered = "routing.reply_delivered"
	// KindRoutingReplyUndeliverable: Tether will not deliver the reply, and
	// says why. The reply stays stored and readable; nothing is dropped.
	KindRoutingReplyUndeliverable = "routing.reply_undeliverable"
)

// RoutingReplyEvent is the payload of both routing.reply_* kinds. It carries
// no reply text: the body is the stored message, read by id.
type RoutingReplyEvent struct {
	ReplyID              string `json:"reply_id"`
	ParentID             string `json:"parent_id"`
	State                string `json:"state"`
	Reason               string `json:"reason,omitempty"`
	Detail               string `json:"detail,omitempty"`
	OriginalSessionID    string `json:"original_session_id"`
	TargetSessionID      string `json:"target_session_id"`
	DeliveredToSessionID string `json:"delivered_to_session_id,omitempty"`
	LogicalAgentID       string `json:"logical_agent_id,omitempty"`
	Actor                string `json:"actor,omitempty"`
}
