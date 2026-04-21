package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chrispian/agent-mux/internal/broker"
)

// fakeBroker records every write and satisfies read-side queries from
// a keyed store. Tests override behavior on specific methods as needed.
type fakeBroker struct {
	created []broker.Envelope
	replied []broker.Envelope
	rows    map[string]*broker.Envelope
	byRecip map[string][]broker.Envelope
	byWork  map[string][]broker.Envelope
	byCorr  map[string][]broker.Envelope // key = workflow_id+"|"+correlation_id
	getErr  error
}

func (f *fakeBroker) CreateEnvelope(_ context.Context, e broker.Envelope) error {
	f.created = append(f.created, e)
	if f.rows == nil {
		f.rows = map[string]*broker.Envelope{}
	}
	cp := e
	f.rows[e.ID] = &cp
	return nil
}

func (f *fakeBroker) ReplyEnvelope(_ context.Context, reply broker.Envelope) error {
	f.replied = append(f.replied, reply)
	if f.rows == nil {
		f.rows = map[string]*broker.Envelope{}
	}
	cp := reply
	f.rows[reply.ID] = &cp
	return nil
}

func (f *fakeBroker) GetEnvelope(id string) (*broker.Envelope, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	e, ok := f.rows[id]
	if !ok {
		return nil, errors.New("sql: no rows in result set")
	}
	return e, nil
}

func (f *fakeBroker) ListEnvelopesByRecipient(recipient string) ([]broker.Envelope, error) {
	return f.byRecip[recipient], nil
}

func (f *fakeBroker) ListEnvelopesByWorkflow(workflowID, correlationID string) ([]broker.Envelope, error) {
	if correlationID != "" {
		return f.byCorr[workflowID+"|"+correlationID], nil
	}
	return f.byWork[workflowID], nil
}

func (f *fakeBroker) WaitForResponse(_ context.Context, _ string) (*broker.Envelope, error) {
	return nil, context.DeadlineExceeded
}

func newBrokerTestHandler(b BrokerService) http.Handler {
	// Broker-only tests don't need LaunchService; nil is allowed because
	// registerSessionRoutes guards on it.
	return NewHandler(Deps{Broker: b})
}

func TestHandleCreateEnvelope_Success(t *testing.T) {
	b := &fakeBroker{}
	body, _ := json.Marshal(EnvelopeCreateRequest{
		Sender:      "alice",
		Recipient:   "bob",
		MessageType: "request",
		Payload:     `{"hello":"world"}`,
	})
	req := httptest.NewRequest(http.MethodPost, "/broker/envelopes", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	newBrokerTestHandler(b).ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	var dto EnvelopeDTO
	if err := json.NewDecoder(rr.Body).Decode(&dto); err != nil {
		t.Fatal(err)
	}
	if dto.ID == "" {
		t.Error("id missing on response")
	}
	if dto.Sender != "alice" || dto.Recipient != "bob" {
		t.Errorf("sender/recipient not preserved: %+v", dto)
	}
	if len(b.created) != 1 {
		t.Fatalf("expected 1 persisted envelope; got %d", len(b.created))
	}
}

func TestHandleCreateEnvelope_BadJSON(t *testing.T) {
	b := &fakeBroker{}
	req := httptest.NewRequest(http.MethodPost, "/broker/envelopes", bytes.NewReader([]byte("{")))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	newBrokerTestHandler(b).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestHandleListEnvelopes_ByRecipient(t *testing.T) {
	env := broker.Envelope{ID: "e1", Recipient: "bob", CreatedAt: "2026-04-19T10:00:00Z"}
	b := &fakeBroker{byRecip: map[string][]broker.Envelope{"bob": {env}}}
	req := httptest.NewRequest(http.MethodGet, "/broker/envelopes?recipient=bob", nil)
	rr := httptest.NewRecorder()
	newBrokerTestHandler(b).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var res EnvelopeListResponse
	if err := json.NewDecoder(rr.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	if len(res.Envelopes) != 1 || res.Envelopes[0].ID != "e1" {
		t.Errorf("result = %+v", res.Envelopes)
	}
}

func TestHandleListEnvelopes_ByWorkflowAndCorrelation(t *testing.T) {
	env := broker.Envelope{ID: "e1", WorkflowID: "w1", CorrelationID: "c1", CreatedAt: "2026-04-19T10:00:00Z"}
	b := &fakeBroker{
		byCorr: map[string][]broker.Envelope{"w1|c1": {env}},
	}
	req := httptest.NewRequest(http.MethodGet, "/broker/envelopes?workflow_id=w1&correlation_id=c1", nil)
	rr := httptest.NewRecorder()
	newBrokerTestHandler(b).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestHandleListEnvelopes_MissingFilter(t *testing.T) {
	b := &fakeBroker{}
	req := httptest.NewRequest(http.MethodGet, "/broker/envelopes", nil)
	rr := httptest.NewRecorder()
	newBrokerTestHandler(b).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestHandleListEnvelopes_ConflictingFilters(t *testing.T) {
	b := &fakeBroker{}
	req := httptest.NewRequest(http.MethodGet, "/broker/envelopes?recipient=bob&workflow_id=w1", nil)
	rr := httptest.NewRecorder()
	newBrokerTestHandler(b).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 on mutually-exclusive filters", rr.Code)
	}
}

func TestHandleGetEnvelope_Found(t *testing.T) {
	env := broker.Envelope{ID: "e1", Sender: "alice", Recipient: "bob", CreatedAt: "2026-04-19T10:00:00Z"}
	b := &fakeBroker{rows: map[string]*broker.Envelope{"e1": &env}}
	req := httptest.NewRequest(http.MethodGet, "/broker/envelopes/e1", nil)
	rr := httptest.NewRecorder()
	newBrokerTestHandler(b).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var dto EnvelopeDTO
	if err := json.NewDecoder(rr.Body).Decode(&dto); err != nil {
		t.Fatal(err)
	}
	if dto.ID != "e1" {
		t.Errorf("id = %q", dto.ID)
	}
}

func TestHandleGetEnvelope_NotFound(t *testing.T) {
	b := &fakeBroker{rows: map[string]*broker.Envelope{}}
	req := httptest.NewRequest(http.MethodGet, "/broker/envelopes/missing", nil)
	rr := httptest.NewRecorder()
	newBrokerTestHandler(b).ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func TestHandleReplyEnvelope_SwapsSenderRecipient(t *testing.T) {
	original := broker.Envelope{
		ID:            "e1",
		Sender:        "alice",
		Recipient:     "bob",
		WorkflowID:    "w1",
		CorrelationID: "c1",
		MessageType:   "request",
	}
	b := &fakeBroker{rows: map[string]*broker.Envelope{"e1": &original}}
	body, _ := json.Marshal(EnvelopeCreateRequest{
		MessageType: "response",
		Payload:     "ok",
	})
	req := httptest.NewRequest(http.MethodPost, "/broker/envelopes/e1/reply", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	newBrokerTestHandler(b).ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	if len(b.replied) != 1 {
		t.Fatalf("expected 1 reply persisted; got %d", len(b.replied))
	}
	reply := b.replied[0]
	if reply.Sender != "bob" || reply.Recipient != "alice" {
		t.Errorf("sender/recipient not swapped: sender=%q recipient=%q", reply.Sender, reply.Recipient)
	}
	if reply.WorkflowID != "w1" {
		t.Errorf("workflow_id not propagated: %q", reply.WorkflowID)
	}
	if reply.CorrelationID != "c1" {
		t.Errorf("correlation_id not propagated: %q", reply.CorrelationID)
	}
	if reply.MessageType != "response" {
		t.Errorf("message_type from body not preserved: %q", reply.MessageType)
	}
}

func TestHandleReplyEnvelope_SeedsCorrelation(t *testing.T) {
	// Original with no correlation_id — reply should use the original's
	// ID as the new correlation_id, seeding the conversation thread.
	original := broker.Envelope{
		ID:        "e1",
		Sender:    "alice",
		Recipient: "bob",
	}
	b := &fakeBroker{rows: map[string]*broker.Envelope{"e1": &original}}
	req := httptest.NewRequest(http.MethodPost, "/broker/envelopes/e1/reply", nil)
	rr := httptest.NewRecorder()
	newBrokerTestHandler(b).ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d", rr.Code)
	}
	if len(b.replied) != 1 || b.replied[0].CorrelationID != "e1" {
		t.Errorf("correlation_id = %q, want original id", b.replied[0].CorrelationID)
	}
}

func TestHandleReplyEnvelope_NotFound(t *testing.T) {
	b := &fakeBroker{rows: map[string]*broker.Envelope{}}
	req := httptest.NewRequest(http.MethodPost, "/broker/envelopes/missing/reply", nil)
	rr := httptest.NewRecorder()
	newBrokerTestHandler(b).ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func TestBrokerRoutes_NotRegisteredWithoutBroker(t *testing.T) {
	// Build a server with Service but no Broker; broker paths 404.
	h := NewHandler(Deps{Service: &fakeLaunchService{}})

	for _, path := range []string{
		"/broker/envelopes",
		"/broker/envelopes/anything",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Errorf("%s: code = %d, want 404", path, rr.Code)
		}
		// Body should be a well-formed error envelope (default mux 404 has
		// no body, but since the route is unregistered it does return plain
		// 404). Either way, status is enough here.
		if strings.Contains(rr.Body.String(), "envelope id required") {
			t.Errorf("%s: unexpected typed envelope error body", path)
		}
	}
}
