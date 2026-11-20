package requestflow

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/hollis-labs/libs/mesh/core/messaging"
	"github.com/hollis-labs/tether/internal/store"
)

type fakeBackend struct {
	Backend
	rows                []Request
	markFailures, marks int
}

func (b *fakeBackend) DueAlerts(context.Context) ([]Request, error) { return b.rows, nil }
func (b *fakeBackend) MarkAlerted(context.Context, string) error {
	if b.markFailures > 0 {
		b.markFailures--
		return errors.New("mark unavailable")
	}
	b.marks++
	return nil
}

type brokenSender struct{ Sender }

func (b brokenSender) Send(context.Context, messaging.Envelope) (messaging.Envelope, error) {
	return messaging.Envelope{}, errors.New("mail unavailable")
}

func TestAlertRetryAfterSendAndBeforeMark(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "alerts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := Request{TaskID: "CW-20261120-9999", RequesterURN: "msg://user/local/operator", RecipientURN: "msg://agent/agent-mux/worker", DueAckAt: time.Now().Add(-time.Minute)}
	b := &fakeBackend{rows: []Request{r}, markFailures: 1}
	a := Alerts{Backend: b, Sender: db.MessagingStore()}
	if err := a.Sweep(t.Context()); err == nil {
		t.Fatal("mark failure hidden")
	}
	if err := a.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	to, _ := messaging.ParseURN(r.RequesterURN)
	page, err := db.MessagingStore().List(t.Context(), to, store.ListFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || b.marks != 1 {
		t.Fatalf("duplicate alert: %d marks %d", page.Total, b.marks)
	}
}
func TestAlertSendFailureRemainsEligible(t *testing.T) {
	b := &fakeBackend{rows: []Request{{TaskID: "CW-20261120-9999", RequesterURN: "msg://user/local/operator"}}}
	if err := (Alerts{Backend: b, Sender: brokenSender{}}).Sweep(t.Context()); err == nil {
		t.Fatal("send failure hidden")
	}
	if b.marks != 0 {
		t.Fatal("undelivered alert marked")
	}
}
