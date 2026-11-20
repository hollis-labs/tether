// Package requestflow connects Tether transport to Torque's durable request tasks.
package requestflow

import (
	"context"
	"time"
)

// Submit is stamped by the daemon, never accepted as an asserted mail sender.
type Submit struct {
	Key             string `json:"send_key"`
	TaskID          string `json:"task_id,omitempty"`
	RequesterURN    string `json:"requester_urn"`
	RecipientURN    string `json:"recipient_urn"`
	Subject         string `json:"subject"`
	Body            string `json:"body"`
	Deliverable     string `json:"deliverable"`
	RecipientOnline bool   `json:"recipient_online"`
}

type Request struct {
	AlertedAt     *time.Time `json:"alerted_at"`
	TaskID        string     `json:"task_id"`
	RequesterURN  string     `json:"requester_urn"`
	RecipientURN  string     `json:"recipient_urn"`
	MailMessageID string     `json:"mail_message_id"`
	State         string     `json:"state"`
	DueAckAt      time.Time  `json:"due_ack_at"`
}

type Transition struct {
	ActorURN string   `json:"author"`
	State    string   `json:"state"`
	Summary  string   `json:"summary"`
	Evidence []string `json:"evidence,omitempty"`
}

// Backend keeps all lifecycle state in Torque. Delivery is retried with stable
// mail keys before acknowledging the alert in Torque.
type Backend interface {
	Submit(context.Context, Submit) (Request, error)
	AttachMessage(context.Context, string, string) error
	Transition(context.Context, string, Transition) error
	DueAlerts(context.Context) ([]Request, error)
	MarkAlerted(context.Context, string) error
}
