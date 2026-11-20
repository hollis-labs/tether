package requestflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hollis-labs/libs/mesh/core/messaging"
)

type Sender interface {
	Send(context.Context, messaging.Envelope) (messaging.Envelope, error)
}

// Alerts has no local clock or state: Torque selects overdue, unacknowledged
// requests. An interruption after Send is safe because the next attempt uses
// exactly the same key and immutable payload.
type Alerts struct {
	Backend Backend
	Sender  Sender
}

func (a Alerts) Deliver(ctx context.Context, r Request) error {
	to, err := messaging.ParseURN(r.RequesterURN)
	if err != nil {
		return fmt.Errorf("request alert requester: %w", err)
	}
	from, _ := messaging.ParseURN("msg://agent/tether/request-watcher")
	body, err := json.Marshal(map[string]string{
		"subject": "Request not acknowledged: " + r.TaskID,
		"body":    fmt.Sprintf("Request %s to %s has not been acknowledged. The acknowledgment deadline was %s. See task:%s for status and comments.", r.TaskID, r.RecipientURN, r.DueAckAt.UTC().Format("2006-01-02T15:04:05Z"), r.TaskID),
		"task_id": r.TaskID,
	})
	if err != nil {
		return err
	}
	_, err = a.Sender.Send(ctx, messaging.Envelope{From: from, To: to, Kind: messaging.MsgKindNotice,
		ContentType: "application/json", Payload: body,
		Metadata: map[string]string{"tether.message_id": "request-unacknowledged:" + r.TaskID, "task_id": r.TaskID, "urgency": "high"},
	})
	if err != nil {
		return fmt.Errorf("send request alert: %w", err)
	}
	reason := "ack_timeout"
	if !r.RecipientOnline {
		reason = "recipient_offline"
	}
	return a.Backend.MarkAlerted(ctx, r.TaskID, reason)
}

func (a Alerts) Sweep(ctx context.Context) error {
	requests, err := a.Backend.DueAlerts(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range requests {
		if err := a.Deliver(ctx, r); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", r.TaskID, err))
		}
	}
	return errors.Join(errs...)
}
