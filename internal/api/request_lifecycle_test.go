package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/libs/mesh/core/messaging"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/requestflow"
	"github.com/hollis-labs/tether/internal/store"
)

type requestBackendFake struct {
	mu                              sync.Mutex
	in                              requestflow.Submit
	row                             requestflow.Request
	attached                        string
	transitions                     []requestflow.Transition
	marks                           int
	attachFail, closeFail, markFail bool
}

func (b *requestBackendFake) Submit(_ context.Context, in requestflow.Submit) (requestflow.Request, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.row.TaskID != "" {
		old := b.in
		old.RecipientOnline = in.RecipientOnline
		if !reflect.DeepEqual(old, in) {
			return requestflow.Request{}, errors.New("conflicting retry")
		}
		return b.row, nil
	}
	b.in = in
	id := in.TaskID
	if id == "" {
		id = "CW-20261120-9999"
	}
	due := time.Now().UTC()
	if in.RecipientOnline {
		due = due.Add(15 * time.Minute)
	}
	b.row = requestflow.Request{TaskID: id, RequesterURN: in.RequesterURN, RecipientURN: in.RecipientURN, State: "submitted", DueAckAt: due}
	return b.row, nil
}
func (b *requestBackendFake) AttachMessage(_ context.Context, _ string, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.attachFail {
		b.attachFail = false
		return errors.New("temporary attach failure")
	}
	b.attached = id
	return nil
}
func (b *requestBackendFake) Transition(_ context.Context, _ string, in requestflow.Transition) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closeFail {
		b.closeFail = false
		return errors.New("temporary close failure")
	}
	if b.row.State != in.State {
		b.transitions = append(b.transitions, in)
		b.row.State = in.State
	}
	return nil
}
func (b *requestBackendFake) DueAlerts(context.Context) ([]requestflow.Request, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.row.AlertedAt == nil && !b.row.DueAckAt.After(time.Now()) {
		return []requestflow.Request{b.row}, nil
	}
	return nil, nil
}
func (b *requestBackendFake) MarkAlerted(context.Context, string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.marks++
	now := time.Now().UTC()
	b.row.AlertedAt = &now
	return nil
}

type failingRequestMail struct {
	MessageStore
	fail bool
}

func (s *failingRequestMail) Send(ctx context.Context, e messaging.Envelope) (messaging.Envelope, error) {
	if s.fail {
		s.fail = false
		return messaging.Envelope{}, errors.New("temporary mail failure")
	}
	return s.MessageStore.Send(ctx, e)
}

func requestHarness(t *testing.T, online bool) (*typedHarness, *requestBackendFake, *failingRequestMail) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "request.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	backend := &requestBackendFake{}
	mail := &failingRequestMail{MessageStore: db.MessagingStore()}
	svc := &fakeLaunchService{}
	if online {
		svc.listRes = []store.SessionRow{{ID: "running", LogicalAgentID: "agt_archie"}}
		svc.runtimeHealthOK = map[string]bool{"running": true}
	}
	cat := &config.Catalog{Agents: map[string]config.Agent{"me": {ID: "me", Handle: "photon-tether", URN: meURN}, "archie": {ID: "archie", URN: archieURN}, "boss": {ID: "boss", URN: bossURN}}}
	srv := httptest.NewServer(NewHandler(Deps{Service: svc, MessageStore: mail, Seats: db, Catalog: fixedCatalog{cat}, Requests: backend, Aliases: db}))
	t.Cleanup(srv.Close)
	return &typedHarness{srv: srv, db: db}, backend, mail
}
func postRequest(t *testing.T, h *typedHarness, as, key, body string) (int, MessagePostResponse, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/messages/post?as="+as, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Idempotency-Key", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out MessagePostResponse
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out, string(raw)
}

const requestBody = `{"kind":"request","to":["@archie"],"subject":"Fix the test","body":"Repair it","deliverable":"Green test"}`

func TestRequestLifecycleSubmitOfflineOwnerAndRunning(t *testing.T) {
	for _, online := range []bool{false, true} {
		t.Run(map[bool]string{false: "offline", true: "running"}[online], func(t *testing.T) {
			h, b, _ := requestHarness(t, online)
			code, out, raw := postRequest(t, h, identity.OperatorID, "submit", requestBody)
			if code != 201 || out.Envelope.TaskID == "" {
				t.Fatalf("post %d %s", code, raw)
			}
			if b.in.RecipientOnline != online || b.in.RequesterURN != identity.OperatorID || b.attached != out.IDs[0] {
				t.Fatalf("backend %+v", b)
			}
			msg, err := h.db.MessagingStore().Get(t.Context(), out.IDs[0])
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]any
			_ = json.Unmarshal(msg.Payload, &payload)
			if payload["task_id"] != b.row.TaskID {
				t.Fatalf("payload %v", payload)
			}
			owner, _ := messaging.ParseURN(identity.OperatorID)
			page, err := h.db.MessagingStore().List(t.Context(), owner, store.ListFilter{Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			if online && (b.marks != 0 || page.Total != 0) {
				t.Fatal("running recipient alerted before deadline")
			}
			if !online && (b.marks != 1 || page.Total != 1) {
				t.Fatalf("offline alert missing marks=%d mail=%d", b.marks, page.Total)
			}
		})
	}
}

func TestRequestLifecycleRepairsPartialFailures(t *testing.T) {
	for _, failure := range []string{"send", "attach"} {
		t.Run(failure, func(t *testing.T) {
			h, b, mail := requestHarness(t, true)
			if failure == "send" {
				mail.fail = true
			} else {
				b.attachFail = true
			}
			code, _, _ := postRequest(t, h, meURN, "same-key", requestBody)
			if code < 500 {
				t.Fatal("injected failure ignored")
			}
			code, out, raw := postRequest(t, h, meURN, "same-key", requestBody)
			if code != 201 {
				t.Fatalf("repair %d %s", code, raw)
			}
			code, retry, raw := postRequest(t, h, meURN, "same-key", requestBody)
			if code != 201 || !reflect.DeepEqual(out.IDs, retry.IDs) {
				t.Fatalf("duplicate %d %s", code, raw)
			}
			to, _ := messaging.ParseURN(archieURN)
			page, err := h.db.MessagingStore().List(t.Context(), to, store.ListFilter{Limit: 10})
			if err != nil || page.Total != 1 {
				t.Fatalf("mail duplicate %d %v", page.Total, err)
			}
			if b.attached != out.IDs[0] {
				t.Fatal("actual message not attached")
			}
			code, _, _ = postRequest(t, h, meURN, "same-key", strings.Replace(requestBody, "Repair it", "Changed work", 1))
			if code < 400 {
				t.Fatal("changed retry accepted")
			}
		})
	}
}

func TestRequestLifecycleReplyRepairAndRecipientOnly(t *testing.T) {
	h, b, _ := requestHarness(t, true)
	code, out, raw := postRequest(t, h, meURN, "request", requestBody)
	if code != 201 {
		t.Fatal(raw)
	}
	reply := `{"kind":"message","in_reply_to":"` + out.IDs[0] + `","to":["@photon-tether"],"status":"done","body":"Fixed","evidence":["https://example.test/pr/1"]}`
	code, _, _ = postRequest(t, h, meURN, "spoof", reply)
	if code < 400 {
		t.Fatal("requester settled recipient work")
	}
	b.closeFail = true
	code, _, _ = postRequest(t, h, archieURN, "reply", reply)
	if code != 502 {
		t.Fatalf("expected close failure, got %d", code)
	}
	code, settled, raw := postRequest(t, h, archieURN, "reply", reply)
	if code != 201 {
		t.Fatalf("reply repair %d %s", code, raw)
	}
	if settled.Envelope.TaskID != b.row.TaskID || len(b.transitions) != 1 || b.transitions[0].ActorURN != archieURN || len(b.transitions[0].Evidence) != 1 {
		t.Fatalf("transition %+v", b.transitions)
	}
	code, _, raw = postRequest(t, h, archieURN, "reply", reply)
	if code != 201 || len(b.transitions) != 1 {
		t.Fatalf("retry %d %s", code, raw)
	}
	sender, _ := messaging.ParseURN(meURN)
	page, err := h.db.MessagingStore().List(t.Context(), sender, store.ListFilter{Limit: 10})
	if err != nil || page.Total != 1 {
		t.Fatalf("reply duplicate %d %v", page.Total, err)
	}
}

func TestRequestLifecycleExplicitTaskAndSingleRecipient(t *testing.T) {
	h, b, _ := requestHarness(t, true)
	multi := strings.Replace(requestBody, `["@archie"]`, `["@archie","@boss"]`, 1)
	code, _, _ := postRequest(t, h, meURN, "multi", multi)
	if code != 400 || b.row.TaskID != "" {
		t.Fatal("ambiguous multi recipient accepted")
	}
	linked := strings.Replace(requestBody, `"kind":"request"`, `"kind":"request","task_id":"CW-20261120-0123"`, 1)
	code, out, raw := postRequest(t, h, meURN, "linked", linked)
	if code != 201 || out.Envelope.TaskID != "CW-20261120-0123" {
		t.Fatalf("link %d %s", code, raw)
	}
}

func TestRequestLifecycleRequiresRepairKey(t *testing.T) {
	h, b, _ := requestHarness(t, true)
	code, _, _ := postRequest(t, h, meURN, "", requestBody)
	if code != 400 || b.row.TaskID != "" {
		t.Fatal("tracked request accepted without repair key")
	}
}
func TestRequestLifecycleCanonicalAlertRetry(t *testing.T) {
	h, b, _ := requestHarness(t, false)
	canonical := "msg://user/local/chrispian"
	if _, _, err := h.db.AddAddressAlias(t.Context(), identity.OperatorID, canonical, "operator"); err != nil {
		t.Fatal(err)
	}
	b.markFail = true
	code, out, raw := postRequest(t, h, identity.OperatorID, "alias-request", requestBody)
	if code != 201 {
		t.Fatalf("submit %d %s", code, raw)
	}
	if b.row.RequesterURN != canonical {
		t.Fatalf("requester not canonical: %s", b.row.RequesterURN)
	}
	if err := (requestflow.Alerts{Backend: b, Sender: h.db.MessagingStore()}).Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	code, retry, raw := postRequest(t, h, identity.OperatorID, "alias-request", requestBody)
	if code != 201 || retry.IDs[0] != out.IDs[0] {
		t.Fatalf("retry %d %s", code, raw)
	}
	to, _ := messaging.ParseURN(canonical)
	page, err := h.db.MessagingStore().List(t.Context(), to, store.ListFilter{Limit: 10})
	if err != nil || page.Total != 1 || b.marks != 1 {
		t.Fatalf("alert retry mail=%d marks=%d err=%v", page.Total, b.marks, err)
	}
}
func TestRequestLifecycleConflictingFinalReplyWithNewKey(t *testing.T) {
	h, b, _ := requestHarness(t, true)
	_, out, _ := postRequest(t, h, meURN, "request", requestBody)
	reply := `{"kind":"message","in_reply_to":"` + out.IDs[0] + `","status":"done","body":"Fixed"}`
	code, _, raw := postRequest(t, h, archieURN, "reply-1", reply)
	if code != 201 {
		t.Fatalf("reply %d %s", code, raw)
	}
	changed := strings.Replace(reply, "Fixed", "Different outcome", 1)
	code, _, _ = postRequest(t, h, archieURN, "reply-2", changed)
	if code < 400 || len(b.transitions) != 1 {
		t.Fatal("conflicting final summary accepted")
	}
	to, _ := messaging.ParseURN(meURN)
	page, _ := h.db.MessagingStore().List(t.Context(), to, store.ListFilter{Limit: 10})
	if page.Total != 1 {
		t.Fatal("conflicting final reply mailed")
	}
}

func TestRequestLifecycleFinalReplyMustReachRequester(t *testing.T) {
	h, b, _ := requestHarness(t, true)
	_, out, _ := postRequest(t, h, meURN, "request", requestBody)
	reply := `{"kind":"message","in_reply_to":"` + out.IDs[0] + `","to":["@boss"],"status":"done","body":"Fixed"}`
	code, _, _ := postRequest(t, h, archieURN, "wrong-recipient", reply)
	if code != 400 || len(b.transitions) != 0 {
		t.Fatal("request settled without informing requester")
	}
}
