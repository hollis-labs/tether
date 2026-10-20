package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/authissuer"
	"github.com/hollis-labs/tether/internal/eventingest"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

// Evidence for the remote service JWT path (CW-20261019-0044, PR 3) that the
// architect asked for on top of the gate matrix.

func serviceIssuer(t *testing.T) *authissuer.Issuer {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "auth")
	keys, err := authissuer.OpenKeyStore(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	state, err := authissuer.OpenState(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	return authissuer.New("tether", keys, state)
}

var evidenceOperator = identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}}

func issueEvents(t *testing.T, is *authissuer.Issuer, app string) (string, authissuer.ServiceRecord) {
	t.Helper()
	tok, rec, err := is.IssueService(evidenceOperator, authissuer.ServiceRequest{App: app, Audience: "tether", Scopes: []string{"events"}})
	if err != nil {
		t.Fatal(err)
	}
	return tok.Token, rec
}

func postEventsAs(h http.Handler, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "http://localhost:9000/events", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// A revocation reaches the hub's own verifier on the very next request through
// the shared deny-list alone: here the live-record check is stubbed to always
// say yes, so only the deny-list can refuse the revoked token.
func TestRevokedServiceJWTIsRefusedByTheDenyListAloneOnTheNextRequest(t *testing.T) {
	is := serviceIssuer(t)
	verifier, err := authissuer.NewVerifier(nil, is)
	if err != nil {
		t.Fatal(err)
	}
	reached := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { reached++; w.WriteHeader(http.StatusOK) })
	h := identity.RemoteMiddleware(nil, nil, next, identity.WithServiceJWT(identity.ServiceJWT{
		Verify:   verifier,
		Known:    func(string) bool { return true },
		Failures: identity.NewAttemptLimiter(50, 100),
	}))
	token, rec := issueEvents(t, is, "cerberus")
	if w := postEventsAs(h, token, "{}"); w.Code != http.StatusOK || reached != 1 {
		t.Fatalf("live token: %d (reached %d)", w.Code, reached)
	}
	if _, err := is.RevokeService(rec.JTI); err != nil {
		t.Fatal(err)
	}
	if w := postEventsAs(h, token, "{}"); w.Code != http.StatusUnauthorized || reached != 1 {
		t.Fatalf("revoked token on the next request: %d (reached %d)", w.Code, reached)
	}
}

// Rotating the service signing key keeps tokens signed by the retired key
// working while their record is live (the key stays published), and the
// emergency stop refuses them at once.
func TestServiceJWTSurvivesRotateKeyAndRevokeAllRefusesIt(t *testing.T) {
	s, _, _ := remoteTestServer(t)
	is := serviceIssuer(t)
	s.Issuer = is
	ing := &fakeRemoteIngest{}
	s.EventIngest = ing
	h := s.RemoteHandler("127.0.0.1:7331")

	before, _ := issueEvents(t, is, "cerberus")
	if w := postEventsAs(h, before, `{"schema_version":"1"}`); w.Code != 200 {
		t.Fatalf("before rotation: %d %s", w.Code, w.Body.String())
	}
	if _, err := is.RotateServiceKey(evidenceOperator, false); err != nil {
		t.Fatal(err)
	}
	if w := postEventsAs(h, before, `{"schema_version":"1"}`); w.Code != 200 {
		t.Fatalf("token signed by the retired key after rotate-key: %d %s", w.Code, w.Body.String())
	}
	// The verifier refreshes its key set at most once a second (MinRefresh), so a
	// token signed by a key published a moment ago is judged after that window.
	time.Sleep(1100 * time.Millisecond)
	after, _ := issueEvents(t, is, "cerberus")
	if w := postEventsAs(h, after, `{"schema_version":"1"}`); w.Code != 200 {
		t.Fatalf("token signed by the new key: %d %s", w.Code, w.Body.String())
	}
	calls := ing.calls
	if _, err := is.RevokeAllServices(""); err != nil {
		t.Fatal(err)
	}
	for name, tok := range map[string]string{"retired key": before, "new key": after} {
		if w := postEventsAs(h, tok, `{"schema_version":"1"}`); w.Code != http.StatusUnauthorized {
			t.Errorf("%s after revoke --all: %d", name, w.Code)
		}
	}
	if ing.calls != calls {
		t.Fatal("a revoked service JWT reached the ingest")
	}
}

// The app a service token publishes as comes from the verified token, never the
// body, end to end through the real remote handler and the real ingest service:
// an event naming another app refuses the whole request and writes nothing; an
// event with no app is stamped with the bound one, and the actor is the service.
func TestRemoteServiceJWTCannotSpoofAppOrActor(t *testing.T) {
	s, _, _ := remoteTestServer(t)
	is := serviceIssuer(t)
	s.Issuer = is
	svc := newRealIngest(t)
	s.EventIngest = svc.service
	h := s.RemoteHandler("127.0.0.1:7331")
	token, _ := issueEvents(t, is, "cerberus")

	event := func(id, app string, extra map[string]any) string {
		m := map[string]any{
			"schema_version": "1", "id": id, "kind": "alert.raised", "time": time.Now().UTC().Format(time.RFC3339Nano),
			"sequence": 1, "generation": 1,
			"source":  map[string]any{"channel": "test", "confidence": "exact"},
			"subject": "msg://alert/cerberus/1", "payload": map[string]any{},
		}
		if app != "" {
			m["app"] = app
		}
		for k, v := range extra {
			m[k] = v
		}
		b, _ := json.Marshal(map[string]any{"events": []any{m}})
		return string(b)
	}

	// Another app, even alongside a valid one: refused, nothing written.
	spoof := `{"events":[` + eventItem(event("evt_00000000000000000000000000000a01", "cerberus", nil)) + `,` +
		eventItem(event("evt_00000000000000000000000000000a02", "torque", nil)) + `]}`
	if w := postEventsAs(h, token, spoof); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "app_mismatch") {
		t.Fatalf("spoofed app: %d %s", w.Code, w.Body.String())
	}
	for _, id := range []string{"evt_00000000000000000000000000000a01", "evt_00000000000000000000000000000a02"} {
		if _, ok, _ := svc.db.EventByEventID(context.Background(), id); ok {
			t.Fatalf("%s was written by a refused request", id)
		}
	}

	// A body-supplied actor is not honored: the stored actor is the service.
	w := postEventsAs(h, token, event("evt_00000000000000000000000000000a03", "cerberus", map[string]any{
		"actor": map[string]any{"kind": "user", "urn": "msg://user/local/operator", "role": "operator"},
	}))
	got, ok, err := svc.db.EventByEventID(context.Background(), "evt_00000000000000000000000000000a03")
	if err != nil || !ok {
		t.Fatalf("event with a body actor was not stored: status %d %s (ok=%v err=%v)", w.Code, w.Body.String(), ok, err)
	}
	if got.Envelope.App != "cerberus" || got.Envelope.Actor.URN != "msg://service/cerberus/dev" {
		t.Fatalf("stored app %q actor %+v", got.Envelope.App, got.Envelope.Actor)
	}
}

type realIngest struct {
	service *eventingest.Service
	db      *store.Store
}

func newRealIngest(t *testing.T) realIngest {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "ingest.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	bus := events.NewBus(events.BusOptions{Persister: db}).(events.OutboxBus)
	t.Cleanup(bus.StartOutbox(context.Background()))
	svc, err := eventingest.New(bus, db, eventingest.Config{AllowUnknownKinds: true})
	if err != nil {
		t.Fatal(err)
	}
	return realIngest{service: svc, db: db}
}

// eventItem unwraps the single event from an {"events":[...]} body.
func eventItem(body string) string {
	var w struct {
		Events []json.RawMessage `json:"events"`
	}
	_ = json.Unmarshal([]byte(body), &w)
	return string(w.Events[0])
}
