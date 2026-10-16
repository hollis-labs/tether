package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

// bootstrapAs serves /sessions/bootstrap with p in the request context, the way the auth
// middleware does; a nil p is the local socket with no principal.
func bootstrapAs(t *testing.T, p *identity.Principal) (*httptest.Server, *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "stamp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	inner := NewHandler(Deps{SessionBootstrap: db})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p != nil {
			r = r.WithContext(identity.WithPrincipal(r.Context(), *p))
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, db
}

func bootstrap(t *testing.T, base string, body map[string]any) int {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(base+"/sessions/bootstrap", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// Only a launcher may assert the task stamp; an agent, a session credential or an unverified
// claim is refused (not silently ignored) and nothing is stored.
func TestTaskStampOnlyTheLauncherMayAssertIt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		p      *identity.Principal
		status int
		stored string
	}{
		{"local socket, no principal", nil, http.StatusOK, "CW-20261016-0155"},
		{"verified operator", &identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator"}, http.StatusOK, "CW-20261016-0155"},
		{"verified service or device credential", &identity.Principal{Verified: true, ID: "msg://device/torque", Kind: "device"}, http.StatusOK, "CW-20261016-0155"},
		{"agent", &identity.Principal{Verified: true, ID: "agent:a", Kind: "agent"}, http.StatusForbidden, ""},
		{"session credential", &identity.Principal{Verified: true, ID: "session:s", Kind: "session", SessionID: "s"}, http.StatusForbidden, ""},
		{"unverified claim", &identity.Principal{Verified: false, ID: "operator", Kind: "operator"}, http.StatusForbidden, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, db := bootstrapAs(t, tc.p)
			if got := bootstrap(t, srv.URL, map[string]any{"session_id": "s1", "task_id": "CW-20261016-0155"}); got != tc.status {
				t.Fatalf("status = %d, want %d", got, tc.status)
			}
			row, err := db.GetSession("s1")
			if tc.status != http.StatusOK {
				if err == nil {
					t.Fatalf("a refused request created a session: %+v", row)
				}
				return
			}
			if err != nil || row.TaskID.String != tc.stored || !row.TaskID.Valid {
				t.Fatalf("stored task = %+v err=%v, want %q", row.TaskID, err, tc.stored)
			}
		})
	}
}

// An agent may still bootstrap its own session; it just cannot name a task.
func TestTaskStampAgentWithoutTaskIDStillBootstraps(t *testing.T) {
	srv, db := bootstrapAs(t, &identity.Principal{Verified: true, ID: "session:s", Kind: "session", SessionID: "s"})
	if got := bootstrap(t, srv.URL, map[string]any{"session_id": "s2"}); got != http.StatusOK {
		t.Fatalf("status = %d", got)
	}
	if row, err := db.GetSession("s2"); err != nil || row.TaskID.Valid {
		t.Fatalf("row = %+v err=%v, want no task", row, err)
	}
}

// The first assertion wins: a later bootstrap of the same session, even by the launcher, never rebinds it.
func TestTaskStampIsNeverRebound(t *testing.T) {
	srv, db := bootstrapAs(t, nil)
	if got := bootstrap(t, srv.URL, map[string]any{"session_id": "s3", "task_id": "CW-20261016-0001"}); got != http.StatusOK {
		t.Fatalf("first: %d", got)
	}
	if got := bootstrap(t, srv.URL, map[string]any{"session_id": "s3", "task_id": "CW-20261016-0002"}); got != http.StatusOK {
		t.Fatalf("second: %d", got)
	}
	if row, err := db.GetSession("s3"); err != nil || row.TaskID.String != "CW-20261016-0001" {
		t.Fatalf("task = %+v err=%v, want the first", row.TaskID, err)
	}
}

func TestTaskStampRejectsMalformedIDs(t *testing.T) {
	srv, db := bootstrapAs(t, nil)
	for _, id := range []string{"-lead", "has space", "a;b", "../x", "new\nline", strings.Repeat("a", 65), "CW-1\x00"} {
		if got := bootstrap(t, srv.URL, map[string]any{"session_id": "bad", "task_id": id}); got != http.StatusBadRequest {
			t.Errorf("task_id %q: status = %d, want 400", id, got)
		}
	}
	if _, err := db.GetSession("bad"); err == nil {
		t.Fatal("a malformed task id created a session")
	}
	for _, id := range []string{"CW-20261016-0155", "T-123", "a", strings.Repeat("a", 64)} {
		if !taskIDPattern.MatchString(id) {
			t.Errorf("task_id %q should be accepted", id)
		}
	}
}
