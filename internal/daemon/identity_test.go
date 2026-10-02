package daemon

import (
	"context"
	"database/sql"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestIdentityHandlerPersistsVerifiedAuditWithoutBusEvent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ids := identity.NewStore(db.DB())
	token, err := ids.Mint(context.Background(), identity.Principal{ID: identity.OperatorID, Kind: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	bus := events.NewBus(events.BusOptions{Persister: db})
	ch, cancel, err := bus.Subscribe(context.Background(), events.Filter{Scopes: []events.Scope{events.ScopeDaemon}})
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	s := &Server{Config: Config{IdentityMode: identity.Observe}, Identity: ids, Publisher: bus}
	defer s.CloseIdentityAudit()
	req := httptest.NewRequest(http.MethodGet, "/missing?secret=not-in-audit", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	out := httptest.NewRecorder()
	s.Handler().ServeHTTP(out, req)
	if out.Code != http.StatusNotFound {
		t.Fatal(out.Code)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var principal, route string
		err := db.DB().QueryRow(`SELECT principal_id,route FROM identity_audit`).Scan(&principal, &route)
		if err == nil {
			if principal != identity.OperatorID || route != "/missing" {
				t.Fatal("wrong attribution")
			}
			break
		}
		if err != sql.ErrNoRows || time.Now().After(deadline) {
			t.Fatal("audit not persisted", err)
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-ch:
		t.Fatal("audit woke an event waiter")
	case <-time.After(50 * time.Millisecond):
	}
}
func TestIdentityAnonymousTrafficDoesNotWakeEventsWait(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	bus := events.NewBus(events.BusOptions{Persister: db})
	s := &Server{Config: Config{IdentityMode: identity.Observe}, Identity: identity.NewStore(db.DB()), Publisher: bus, Bus: bus, Catalog: stubCatalogLoader{}}
	defer s.CloseIdentityAudit()
	live := httptest.NewServer(s.Handler())
	defer live.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, live.URL+"/events/stream?scope=daemon", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := live.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatal("SSE/Flusher changed", resp.StatusCode)
	}
	received := make(chan error, 1)
	go func() { buf := make([]byte, 1); _, err := resp.Body.Read(buf); received <- err }()
	for range 10 {
		s.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/missing", nil))
	}
	select {
	case err := <-received:
		t.Fatal("anonymous traffic woke or closed events watch", err)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	_ = resp.Body.Close()
	<-received
	var exists bool
	if err := db.DB().QueryRow(`SELECT EXISTS(SELECT 1 FROM identity_audit)`).Scan(&exists); err != nil || exists {
		t.Fatal("anonymous traffic audited", err)
	}
}
