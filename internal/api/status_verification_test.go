package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

// statusIdentityHarness is the typed-message harness with verified principals and an identity mode.
func statusIdentityHarness(t *testing.T, mode identity.Mode) (func(method, path, principal, body string) (int, []byte), *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "status.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cat := &config.Catalog{Agents: map[string]config.Agent{
		"photon-tether": {ID: "photon-tether", URN: meURN},
		"archie":        {ID: "archie", URN: archieURN},
		"boss":          {ID: "boss", URN: bossURN, Roles: []string{"manager"}},
	}}
	h := NewHandler(Deps{Service: &fakeLaunchService{}, MessageStore: db.MessagingStore(), Seats: db, Catalog: fixedCatalog{cat},
		IdentityMode: mode, Bus: events.NewBus(events.BusOptions{Persister: db})})
	principals := map[string]identity.Principal{
		"boss":     {Verified: true, ID: "msg://session/local/boss", Kind: "session", Addresses: []string{bossURN}},
		"me":       {Verified: true, ID: "msg://session/local/me", Kind: "session", Addresses: []string{meURN}},
		"operator": {Verified: true, ID: identity.OperatorID, Kind: "operator"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := principals[r.Header.Get("X-Test-Principal")]; ok {
			r = r.WithContext(identity.WithPrincipal(r.Context(), p))
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return func(method, path, principal, body string) (int, []byte) {
		req, _ := http.NewRequest(method, srv.URL+path, bytes.NewReader([]byte(body)))
		if principal != "" {
			req.Header.Set("X-Test-Principal", principal)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		return resp.StatusCode, buf.Bytes()
	}, db
}

// CW-20261114-0130: in observe mode (production's) an unverified ?as= claim was recorded but not refused,
// so naming a manager seat let anyone set any seat's status. A claim that does not verify now sets
// nothing in every mode; a verified manager, the seat itself and the operator still do, and the status
// records who set it and whether the hub verified that.
func TestStatusSetRequiresAVerifiedSetter(t *testing.T) {
	for _, mode := range []identity.Mode{identity.Observe, identity.Enforce} {
		t.Run(string(mode), func(t *testing.T) {
			do, db := statusIdentityHarness(t, mode)
			card := func() StatusCard {
				code, raw := do(http.MethodGet, "/status/@photon-tether", "", "")
				var c StatusCard
				if code != http.StatusOK || json.Unmarshal(raw, &c) != nil {
					t.Fatalf("card: %d %s", code, raw)
				}
				return c
			}
			// "me" names the manager seat it does not hold: refused, for another seat and for that seat.
			for _, body := range []string{`{"target":"@photon-tether","state":"paused"}`, `{"state":"paused"}`} {
				if code, raw := do(http.MethodPost, "/status?as="+bossURN, "me", body); code != http.StatusForbidden {
					t.Fatalf("unverified ?as=<manager> %s: %d %s", body, code, raw)
				}
			}
			if _, found, _ := db.GetSeatStatus(context.Background(), meURN); found {
				t.Fatal("a refused set stored a status")
			}
			// Without a principal: refused outside identity off.
			if code, raw := do(http.MethodPost, "/status?as="+meURN, "", `{"state":"paused"}`); code != http.StatusForbidden {
				t.Fatalf("anonymous: %d %s", code, raw)
			}
			// The seat itself, stamped from its own address.
			if code, raw := do(http.MethodPost, "/status", "me", `{"state":"working"}`); code != http.StatusOK {
				t.Fatalf("own seat: %d %s", code, raw)
			}
			if c := card(); c.Declared == nil || c.Declared.SetByVerification != "stamped" || c.Declared.SetByPrincipal != "msg://session/local/me" {
				t.Fatalf("own set recorded %+v", c.Declared)
			}
			// A verified manager sets another seat.
			if code, raw := do(http.MethodPost, "/status?as="+bossURN, "boss", `{"target":"@photon-tether","state":"paused","reason":"hold"}`); code != http.StatusOK {
				t.Fatalf("verified manager: %d %s", code, raw)
			}
			if c := card(); c.Declared.SetBy != bossURN || c.Declared.SetByVerification != "verified" || c.Declared.SetByPrincipal != "msg://session/local/boss" {
				t.Fatalf("manager set recorded %+v", c.Declared)
			}
			// The operator sets any seat.
			if code, raw := do(http.MethodPost, "/status", "operator", `{"target":"@photon-tether","state":"available"}`); code != http.StatusOK {
				t.Fatalf("operator: %d %s", code, raw)
			}
			var payload map[string]any
			var raw string
			if err := db.DB().QueryRow(`SELECT payload_json FROM events WHERE kind=? ORDER BY id DESC LIMIT 1`, KindSeatStatus).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			_ = json.Unmarshal([]byte(raw), &payload)
			if payload["set_by_verification"] != "stamped" || payload["set_by_principal"] != identity.OperatorID {
				t.Fatalf("seat.status = %s", raw)
			}
		})
	}
}
