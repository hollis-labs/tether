package broker

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

type operationBackend struct {
	written                   []Envelope
	original                  *Envelope
	writeErr, getErr, waitErr error
	waited                    string
	waitDeadline              bool
}

func (b *operationBackend) CreateEnvelope(_ context.Context, e Envelope) error {
	if b.writeErr != nil {
		return b.writeErr
	}
	b.written = append(b.written, e)
	return nil
}
func (b *operationBackend) ReplyEnvelope(ctx context.Context, e Envelope) error {
	return b.CreateEnvelope(ctx, e)
}
func (b *operationBackend) GetEnvelope(string) (*Envelope, error) { return b.original, b.getErr }
func (b *operationBackend) WaitForResponse(ctx context.Context, correlation string) (*Envelope, error) {
	b.waited = correlation
	_, b.waitDeadline = ctx.Deadline()
	return b.original, b.waitErr
}
func deterministicOperations(b *operationBackend) *Operations {
	o := NewOperations(b)
	n := byte(0)
	o.newID = func() (uuid.UUID, error) { n++; id := uuid.UUID{}; id[15] = n; return id, nil }
	o.now = func() time.Time { return time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC) }
	return o
}
func assertOperationFailure(t *testing.T, err error, code, message string) {
	t.Helper()
	var e *OperationError
	if !errors.As(err, &e) || e.Code != code || e.Message != message {
		t.Fatalf("error=%v, want %s / %s", err, code, message)
	}
}
func TestOperationsCorrelationAndAuthoredFields(t *testing.T) {
	for _, kind := range []string{"", TypeNotice, TypeRequest, TypeResponse} {
		t.Run(kind, func(t *testing.T) {
			b := &operationBackend{}
			o := deterministicOperations(b)
			req := CreateRequest{Sender: "alice", Recipient: "bob", WorkflowID: "workflow", CorrelationID: "authored", MessageType: kind, Priority: 3, Payload: `{"body":"private"}`, AuditJSON: `{"audit":true}`}
			e, err := o.Create(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			correlation := "authored"
			if kind == TypeRequest {
				correlation = "00000000-0000-0000-0000-000000000002"
			}
			want := Envelope{ID: "00000000-0000-0000-0000-000000000001", Sender: "alice", Recipient: "bob", WorkflowID: "workflow", CorrelationID: correlation, MessageType: kind, Priority: 3, Payload: req.Payload, AuditJSON: req.AuditJSON, CreatedAt: "2026-10-02T12:00:00Z"}
			if !reflect.DeepEqual(*e, want) || !reflect.DeepEqual(b.written, []Envelope{want}) {
				t.Fatalf("envelope=%+v, persisted=%+v, want %+v", e, b.written, want)
			}
		})
	}
}
func TestOperationsReplyInheritsOnlyOriginalThreadAndParties(t *testing.T) {
	for _, correlation := range []string{"", "thread"} {
		b := &operationBackend{}
		o := deterministicOperations(b)
		original := Envelope{ID: "original", Sender: "alice", Recipient: "bob", WorkflowID: "workflow", CorrelationID: correlation}
		req := CreateRequest{Sender: "forged", Recipient: "forged", WorkflowID: "forged", CorrelationID: "forged", MessageType: "invalid-but-legacy-reply-accepts", Priority: 4, Payload: "reply", AuditJSON: "audit"}
		reply, err := o.Reply(context.Background(), original, req)
		if err != nil {
			t.Fatal(err)
		}
		wantCorrelation := correlation
		if correlation == "" {
			wantCorrelation = "original"
		}
		if reply.Sender != "bob" || reply.Recipient != "alice" || reply.WorkflowID != "workflow" || reply.CorrelationID != wantCorrelation || reply.MessageType != req.MessageType || reply.Payload != req.Payload || reply.Priority != 4 || reply.AuditJSON != "audit" {
			t.Fatalf("reply=%+v", reply)
		}
	}
}
func TestOperationsRequestForcesTypeAndWaitUsesAssignedCorrelation(t *testing.T) {
	b := &operationBackend{original: &Envelope{ID: "response"}}
	o := deterministicOperations(b)
	e, err := o.Request(context.Background(), CreateRequest{MessageType: "invalid", CorrelationID: "forged"})
	if err != nil {
		t.Fatal(err)
	}
	if e.MessageType != TypeRequest || e.CorrelationID == "forged" || e.ID == e.CorrelationID {
		t.Fatalf("request=%+v", e)
	}
	response, err := o.Wait(context.Background(), e.CorrelationID, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if response != b.original || b.waited != e.CorrelationID || !b.waitDeadline {
		t.Fatalf("wait did not use assigned correlation/deadline: %+v", b)
	}
	b.waitErr = errors.New("backend disconnected")
	_, err = o.Wait(context.Background(), e.CorrelationID, 0)
	assertOperationFailure(t, err, "timeout", "no response received within 0s")
}
func TestOperationsFailureOrderingAndClassification(t *testing.T) {
	b := &operationBackend{}
	o := deterministicOperations(b)
	o.newID = func() (uuid.UUID, error) { return uuid.Nil, errors.New("entropy failed") }
	_, err := o.Create(context.Background(), CreateRequest{MessageType: "invalid"})
	assertOperationFailure(t, err, "internal_error", "generate id: entropy failed")
	o = deterministicOperations(b)
	_, err = o.Create(context.Background(), CreateRequest{MessageType: "invalid"})
	assertOperationFailure(t, err, "invalid_request", `unknown message_type "invalid"; valid: request, response, notice, escalation, handoff, status_update`)
	if len(b.written) != 0 {
		t.Fatal("invalid envelope persisted")
	}
	b.writeErr = errors.New("response requires correlation_id")
	_, err = o.Create(context.Background(), CreateRequest{MessageType: TypeResponse})
	assertOperationFailure(t, err, "invalid_request", b.writeErr.Error())
	_, err = o.Request(context.Background(), CreateRequest{})
	assertOperationFailure(t, err, "internal_error", b.writeErr.Error())
	_, err = o.Reply(context.Background(), Envelope{ID: "orig"}, CreateRequest{})
	assertOperationFailure(t, err, "internal_error", b.writeErr.Error())
	b.getErr = errors.New("sql: no rows in result set")
	_, err = o.Original("missing")
	assertOperationFailure(t, err, "not_found", "envelope not found")
	b.getErr = errors.New("read failed")
	_, err = o.Original("missing")
	assertOperationFailure(t, err, "internal_error", "read failed")
}
func TestRequestOptionsPreserveWaitTimeoutPolicy(t *testing.T) {
	for _, tc := range []struct {
		wait, timeout string
		blocking      bool
		duration      time.Duration
	}{
		{"", "", false, 30 * time.Second}, {"true", "", true, 30 * time.Second}, {"1", "", true, 30 * time.Second}, {"", "2s", true, 2 * time.Second}, {"false", "2s", false, 2 * time.Second}, {"TRUE", "2s", false, 2 * time.Second}, {"true", "-1s", true, -time.Second},
	} {
		opts, err := ParseRequestOptions(tc.wait, tc.timeout)
		if err != nil {
			t.Fatal(err)
		}
		if opts.Blocking != tc.blocking || opts.Timeout != tc.duration {
			t.Fatalf("wait=%q timeout=%q: %+v", tc.wait, tc.timeout, opts)
		}
	}
	_, err := ParseRequestOptions("false", "oops")
	assertOperationFailure(t, err, "invalid_request", `invalid timeout: time: invalid duration "oops"`)
}
