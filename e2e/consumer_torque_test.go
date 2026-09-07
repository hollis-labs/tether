package e2e

// consumer_torque_test.go — T11's "Torque task/session" fixture
// consumer. Torque itself is a separate repository this task must not
// edit; no Torque integration code exists in this repo to reference, so
// this shape-matches how a task tracker would use Tether: assign work to
// a task-runner agent via a request-kind message, watch the runner
// consume it and send a status-update reply, and use T09's trace
// tooling (the same evidence surface a real Torque would poll) to
// confirm the full request/response lifecycle completed durably.

import (
	"encoding/json"
	"testing"

	"github.com/hollis-labs/go-messaging/delivery"
)

func TestConsumer_Torque_TaskAssignmentRequestResponseLifecycleIsTraceable(t *testing.T) {
	d := StartFixtureDaemon(t)
	c := d.Client()

	torque := registerAgent(t, c, "e2e-torque-tracker")
	runner := registerAgent(t, c, "e2e-torque-task-runner")

	assignmentPayload, err := json.Marshal(map[string]string{
		"task_id": "CW-e2e-0001",
		"action":  "run tests",
	})
	if err != nil {
		t.Fatalf("marshal assignment payload: %v", err)
	}
	assignment, err := c.MessageSend(ctx(), messageSendRequestOfKind("request", torque, runner, assignmentPayload))
	if err != nil {
		t.Fatalf("send task assignment: %v", err)
	}

	if err := c.MessageConsume(ctx(), assignment.ID, runner); err != nil {
		t.Fatalf("runner consumes the assignment: %v", err)
	}

	statusPayload, err := json.Marshal(map[string]string{
		"task_id": "CW-e2e-0001",
		"status":  "done",
	})
	if err != nil {
		t.Fatalf("marshal status payload: %v", err)
	}
	statusMsg := messageSendRequestOfKind("status_update", runner, torque, statusPayload)
	statusMsg.InReplyTo = assignment.ID
	statusUpdate, err := c.MessageSend(ctx(), statusMsg)
	if err != nil {
		t.Fatalf("send status update: %v", err)
	}
	if err := c.MessageConsume(ctx(), statusUpdate.ID, torque); err != nil {
		t.Fatalf("torque consumes the status update: %v", err)
	}

	// Torque's own tracking loop would poll trace evidence like this to
	// confirm the assignment was genuinely, durably delivered and
	// consumed -- not just that a Send call returned 200.
	assignmentTrace, err := c.MessageTrace(ctx(), assignment.ID)
	if err != nil {
		t.Fatalf("trace assignment: %v", err)
	}
	if assignmentTrace.Status != string(delivery.DeliveryDelivered) {
		t.Fatalf("assignment trace status = %q, want delivered", assignmentTrace.Status)
	}
	if assignmentTrace.From != torque || assignmentTrace.To != runner {
		t.Fatalf("assignment trace from/to = %s/%s, want %s/%s", assignmentTrace.From, assignmentTrace.To, torque, runner)
	}

	statusTrace, err := c.MessageTrace(ctx(), statusUpdate.ID)
	if err != nil {
		t.Fatalf("trace status update: %v", err)
	}
	if statusTrace.Status != string(delivery.DeliveryDelivered) {
		t.Fatalf("status update trace status = %q, want delivered", statusTrace.Status)
	}

	got, err := c.MessageGet(ctx(), statusUpdate.ID, torque)
	if err != nil {
		t.Fatalf("get status update: %v", err)
	}
	if got.InReplyTo != assignment.ID {
		t.Fatalf("status update in_reply_to = %q, want %q -- Torque correlates responses to assignments via this field", got.InReplyTo, assignment.ID)
	}
}
