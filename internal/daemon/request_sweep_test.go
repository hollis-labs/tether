package daemon

import (
	"context"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hollis-labs/libs/mesh/core/messaging"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/requestflow"
)

type countingRequestBackend struct {
	requestflow.Backend
	calls atomic.Int64
}

func (b *countingRequestBackend) DueAlerts(context.Context) ([]requestflow.Request, error) {
	b.calls.Add(1)
	return nil, nil
}

type unusedRequestStore struct{ api.MessageStore }

func (s unusedRequestStore) Send(context.Context, messaging.Envelope) (messaging.Envelope, error) {
	panic("no due rows")
}
func TestServerRunStartsRequestSweepAndStops(t *testing.T) {
	dir := shortTempDir(t)
	previous := requestSweepInterval
	requestSweepInterval = 10 * time.Millisecond
	defer func() { requestSweepInterval = previous }()
	b := &countingRequestBackend{}
	s := &Server{Config: Config{ListenAddr: "unix:" + dir + "/s.sock", PIDFile: dir + "/d.pid", ShutdownTimeout: time.Second}, Requests: b, MessageStore: unusedRequestStore{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for b.calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if b.calls.Load() < 2 {
		t.Fatal("request sweep did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown blocked")
	}
	count := b.calls.Load()
	time.Sleep(50 * time.Millisecond)
	if b.calls.Load() > count+1 {
		t.Fatal("request sweep continued after cancellation")
	}
}

type routeRequestBackend struct {
	requestflow.Backend
	submitted bool
}

func (b *routeRequestBackend) Submit(_ context.Context, in requestflow.Submit) (requestflow.Request, error) {
	b.submitted = true
	return requestflow.Request{TaskID: "CW-20261120-9999", State: "submitted", DueAckAt: time.Now().Add(time.Hour)}, nil
}
func (b *routeRequestBackend) AttachMessage(context.Context, string, string) error { return nil }

type requestCatalog struct{ cat *config.Catalog }

func (c requestCatalog) Load() (*config.Catalog, error) { return c.cat, nil }
func TestServerHandlerWiresRequestBackend(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "requests.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	b := &routeRequestBackend{}
	cat := &config.Catalog{Agents: map[string]config.Agent{"worker": {ID: "worker", URN: "msg://agent/agent-mux/worker"}}}
	s := &Server{Config: Config{IdentityMode: identity.Off}, Catalog: requestCatalog{cat}, MessageStore: db.MessagingStore(), Requests: b}
	req := httptest.NewRequest(http.MethodPost, "/messages/post?as=msg://user/local/operator", strings.NewReader(`{"kind":"request","to":["@worker"],"subject":"Work","body":"Do work","deliverable":"Result"}`))
	req.Header.Set("Idempotency-Key", "daemon-route")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != 201 || !b.submitted || !strings.Contains(w.Body.String(), `"task_id":"CW-20261120-9999"`) {
		t.Fatalf("request route %d %s", w.Code, w.Body.String())
	}
}
