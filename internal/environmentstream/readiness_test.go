package environmentstream

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
)

const cutoverEnvironment = "5def3319-adb3-44ab-b280-9115c6301ae3"

func TestEventCutoverLegacyGoldenReadinessAndNoMixedIDs(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	for range 3 {
		if _, err := db.DB().Exec(`INSERT INTO events(scope,at,kind,payload_json) VALUES('daemon','2026-01-01T00:00:00Z','legacy','{}')`); err != nil {
			t.Fatal(err)
		}
	}
	bus := events.NewBus(events.BusOptions{Persister: db})
	legacy := New(cutoverEnvironment, db, bus)
	if legacy.StoredIDs != nil || legacy.EventIdentityHealth(ctx) != nil {
		t.Fatal("legacy constructor activated")
	}
	if _, err := NewStored(ctx, cutoverEnvironment, db, bus); !errors.Is(err, store.ErrEventIdentityUnseeded) {
		t.Fatal(err)
	}
	before, err := db.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	p, err := db.SeedEventIdentity(ctx, cutoverEnvironment)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewStored(ctx, cutoverEnvironment, db, bus)
	if err != nil {
		t.Fatal(err)
	}
	if s.EnvironmentID != p.EnvironmentID || s.StoredIDs.Cutover != p.Cutover {
		t.Fatal("unfrozen startup coordinates")
	}
	if _, err := NewStored(ctx, "7f67df10-16ee-48a5-86cc-e063f69366a9", db, bus); !errors.Is(err, store.ErrEventIdentityMismatch) {
		t.Fatal("mismatched server accepted", err)
	}
	// Even an oversized catch-up/gap request and snapshot must wait; neither
	// legacy Map nor a snapshot can bypass pending legacy history.
	s.MaxCatchUp = 1
	gapWindow, err := db.EnvironmentEvents(ctx, 0, 1)
	if err != nil || gapWindow.GapReason == "" || len(gapWindow.Events) != 0 {
		t.Fatal("gap carried unbackfilled rows", gapWindow, err)
	}
	for _, snapshot := range []bool{false, true} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/?after_seq=0", nil)
		if snapshot {
			s.Snapshot(rec, req, "")
		} else {
			s.Stream(rec, req, "")
		}
		if rec.Code != 503 || rec.Header().Get("Retry-After") != "1" || !strings.Contains(rec.Body.String(), "backfill_in_progress") || strings.Contains(rec.Body.String(), "evt_") {
			t.Fatalf("mixed/unready response %d %s", rec.Code, rec.Body.String())
		}
	}
	if _, err := db.BackfillEventIDs(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if h := s.EventIdentityHealth(ctx); h.Ready || h.Progress == nil || h.Progress.Remaining != 2 {
		t.Fatal(h)
	}
	if _, err := db.BackfillEventIDs(ctx, 1000); err != nil {
		t.Fatal(err)
	}
	if h := s.EventIdentityHealth(ctx); !h.Ready || h.Progress.Cutover != p.Cutover {
		t.Fatal(h)
	}
	after, err := db.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	for i, e := range after {
		old, err := Map(cutoverEnvironment, before[i])
		if err != nil {
			t.Fatal(err)
		}
		mapped, err := s.StoredIDs.MapStored(cutoverEnvironment, e)
		if err != nil || old.ID != e.EventID || mapped.ID != old.ID || mapped.Cursor != old.Cursor {
			t.Fatal("legacy byte/cursor identity changed", old, e, mapped, err)
		}
		raw, _ := json.Marshal(mapped)
		var wire map[string]any
		_ = json.Unmarshal(raw, &wire)
		if _, ok := wire["id"].(string); !ok {
			t.Fatal("non-string wire id")
		}
	}
	rec := httptest.NewRecorder()
	s.Snapshot(rec, httptest.NewRequest(http.MethodGet, "/", nil), "")
	if rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	// Fresh service gets the persisted cutover, even after more producer rows.
	if err := bus.Publish(ctx, events.Event{Scope: events.ScopeDaemon, Kind: "new", PayloadJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	again, err := NewStored(ctx, cutoverEnvironment, db, bus)
	if err != nil || again.StoredIDs.Cutover != p.Cutover {
		t.Fatal(again, err)
	}
}

func TestEventCutoverHealthOmitsUnavailableProgress(t *testing.T) {
	s := &Server{EnvironmentID: cutoverEnvironment, StoredIDs: &StoredIDProjection{}, identityProgress: func(context.Context) (store.EventIdentityProgress, error) {
		return store.EventIdentityProgress{}, errors.New("private error body")
	}}
	raw, _ := json.Marshal(s.EventIdentityHealth(context.Background()))
	if string(raw) != `{"ready":false}` {
		t.Fatal(string(raw))
	}
	rec := httptest.NewRecorder()
	s.Snapshot(rec, httptest.NewRequest(http.MethodGet, "/", nil), "")
	if rec.Code != 503 || strings.Contains(rec.Body.String(), "private") {
		t.Fatal(rec.Code, rec.Body.String())
	}
}

func TestEventCutoverReadyHTTPStreamUsesStoredOpaqueIDs(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.SeedEventIdentity(ctx, cutoverEnvironment); err != nil {
		t.Fatal(err)
	}
	bus := events.NewBus(events.BusOptions{Persister: db})
	if err := bus.Publish(ctx, events.Event{Scope: events.ScopeDaemon, Kind: "new", PayloadJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	es, err := db.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewStored(ctx, cutoverEnvironment, db, bus)
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.Stream(w, r, "") }))
	defer srv.Close()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"?after_seq=0", nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") || !strings.Contains(line, es[0].EventID) {
			continue
		}
		if strings.Contains(line, `"id":"`+cutoverEnvironment+`:1"`) || !strings.Contains(line, `"cursor":"1"`) {
			t.Fatal("stored wire identity/cursor", line)
		}
		return
	}
	t.Fatal("missing stored event", scanner.Err())
}

func TestEventCutoverLegacyAndStoredHTTPCompatibility(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	if _, err := db.DB().Exec(`INSERT INTO events(scope,at,kind,payload_json) VALUES('daemon','2026-01-01T00:00:00Z','legacy','{"n":1}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SeedEventIdentity(ctx, cutoverEnvironment); err != nil {
		t.Fatal(err)
	}
	if _, err := db.BackfillEventIDs(ctx, 1000); err != nil {
		t.Fatal(err)
	}
	bus := events.NewBus(events.BusOptions{Persister: db})
	stored, err := NewStored(ctx, cutoverEnvironment, db, bus)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []*Server{New(cutoverEnvironment, db, bus), stored} {
		func() {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.Stream(w, r, "") }))
			defer srv.Close()
			requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(requestCtx, http.MethodGet, srv.URL+"?after_seq=0", nil)
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			scanner := bufio.NewScanner(resp.Body)
			sseID := ""
			for scanner.Scan() {
				line := scanner.Text()
				if strings.HasPrefix(line, "id:") {
					sseID = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
				}
				if !strings.HasPrefix(line, "data:") {
					continue
				}
				var ev Event
				if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &ev) != nil || ev.Kind != "legacy" {
					continue
				}
				if ev.ID != cutoverEnvironment+":1" || ev.Seq != 1 || ev.Cursor != "1" || sseID != "1" || string(ev.Payload) != `{"n":1}` {
					t.Fatal("compatibility changed", ev, sseID)
				}
				return
			}
			t.Fatal("compatibility stream missing legacy event", scanner.Err())
		}()
	}
}
