package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

func TestIdentityHandlerPersistsVerifiedAuditAndEvent(t *testing.T) {
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
	var carried string
	server := &Server{Config: Config{IdentityMode: identity.Observe}, Identity: ids, Publisher: bus,
		A2A: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := identity.FromContext(r.Context())
			if ok {
				carried = p.ID
			}
			w.WriteHeader(http.StatusNoContent)
		}),
	}
	req := httptest.NewRequest(http.MethodPost, "/a2a/call?secret=not-in-audit", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	out := httptest.NewRecorder()
	server.Handler().ServeHTTP(out, req)
	if out.Code != http.StatusNoContent || carried != identity.OperatorID {
		t.Fatalf("status=%d principal=%s", out.Code, carried)
	}
	var principal, route string
	if err := db.DB().QueryRow(`SELECT principal_id, route FROM identity_audit`).Scan(&principal, &route); err != nil {
		t.Fatal(err)
	}
	if principal != identity.OperatorID || route != "/a2a" {
		t.Fatalf("audit principal=%s route=%s", principal, route)
	}
	rows, err := db.QueryEvents(store.EventFilter{Kinds: []string{events.KindIdentityObserved}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("identity event count=%d", len(rows))
	}
	var observed identity.Observation
	if err := json.Unmarshal([]byte(rows[0].PayloadJSON), &observed); err != nil {
		t.Fatal(err)
	}
	if observed.PrincipalID != identity.OperatorID || observed.Authentication != "verified" {
		t.Fatalf("event=%+v", observed)
	}
	if strings.Contains(rows[0].PayloadJSON, token) || strings.Contains(rows[0].PayloadJSON, "not-in-audit") {
		t.Fatal("secret leaked to event")
	}
}
