package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

// bootstrapAs serves /sessions/bootstrap with p in the request context, the way the auth
// middleware does; a nil p is the local socket with no principal.
func bootstrapAs(t *testing.T, p *identity.Principal) (*httptest.Server, *store.Store) {
	srv, db, _ := bootstrapAsAudited(t, p)
	return srv, db
}

// bootstrapAsAudited also returns the identity_audit events the handler recorded.
func bootstrapAsAudited(t *testing.T, p *identity.Principal) (*httptest.Server, *store.Store, *[]identity.Observation) {
	t.Helper()
	var audited []identity.Observation
	var mu sync.Mutex
	db, err := store.Open(filepath.Join(t.TempDir(), "stamp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	inner := NewHandler(Deps{SessionBootstrap: db, IdentityAudit: func(_ context.Context, o identity.Observation) error {
		mu.Lock()
		defer mu.Unlock()
		audited = append(audited, o)
		return nil
	}})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p != nil {
			r = r.WithContext(identity.WithPrincipal(r.Context(), *p))
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, db, &audited
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

// Architect ruling for tether#379: the verified local operator may assert the stamp; an agent or
// session credential is refused (403, nothing stored); any other caller has the field dropped, the
// launch still succeeds without a stamp, and the drop is audited.
func TestTaskStampPolicy(t *testing.T) {
	const id = "CW-20261016-0155"
	for _, tc := range []struct {
		name   string
		p      *identity.Principal
		status int
		stored bool
		audit  bool
	}{
		{"verified operator", &identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator"}, http.StatusOK, true, false},
		{"agent", &identity.Principal{Verified: true, ID: "agent:a", Kind: "agent"}, http.StatusForbidden, false, false},
		{"session credential", &identity.Principal{Verified: true, ID: "session:s", Kind: "session", SessionID: "s"}, http.StatusForbidden, false, false},
		{"worker device", &identity.Principal{Verified: true, ID: "msg://device/w", Kind: "device"}, http.StatusOK, false, true},
		{"other service kind", &identity.Principal{Verified: true, ID: "svc:tangent-pilot", Kind: "service"}, http.StatusOK, false, true},
		{"unknown future kind", &identity.Principal{Verified: true, ID: "x", Kind: "brand-new-kind"}, http.StatusOK, false, true},
		{"operator-minted narrow token", &identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator", TokenID: "jti-1"}, http.StatusOK, false, true},
		{"unverified operator claim", &identity.Principal{Verified: false, ID: identity.OperatorID, Kind: "operator"}, http.StatusOK, false, true},
		{"no principal", nil, http.StatusOK, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, db, audited := bootstrapAsAudited(t, tc.p)
			if got := bootstrap(t, srv.URL, map[string]any{"session_id": "s1", "task_id": id}); got != tc.status {
				t.Fatalf("status = %d, want %d", got, tc.status)
			}
			row, err := db.GetSession("s1")
			if tc.status != http.StatusOK {
				if err == nil {
					t.Fatalf("a refused request created a session: %+v", row)
				}
				return
			}
			if err != nil {
				t.Fatalf("the launch must still succeed: %v", err)
			}
			if tc.stored != row.TaskID.Valid || (tc.stored && row.TaskID.String != id) {
				t.Fatalf("stored task = %+v, want stored=%v", row.TaskID, tc.stored)
			}
			dropped := 0
			for _, o := range *audited {
				if o.Event == eventTaskIDDropped {
					dropped++
					wantID, wantAuth := "", "none"
					if tc.p != nil {
						wantID, wantAuth = tc.p.ID, map[bool]string{true: "verified", false: "claimed"}[tc.p.Verified]
					}
					if o.PrincipalID != wantID || o.Authentication != wantAuth {
						t.Fatalf("audit row = %+v, want principal %q authentication %q", o, wantID, wantAuth)
					}
				}
			}
			if (dropped == 1) != tc.audit || dropped > 1 {
				t.Fatalf("task_id_dropped audit rows = %d, want audited=%v", dropped, tc.audit)
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
	srv, db := bootstrapAs(t, &identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator"})
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
	srv, db := bootstrapAs(t, &identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator"})
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

// POST /sessions follows the same policy, and the service sees only the task id the policy let through.
func TestLaunchTaskStampPolicy(t *testing.T) {
	op := identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator"}
	dev := identity.Principal{Verified: true, ID: "msg://device/w", Kind: "device"}
	agent := identity.Principal{Verified: true, ID: "agent:a", Kind: "agent"}
	for _, tc := range []struct {
		name   string
		p      identity.Principal
		code   int
		taskID string
	}{
		{"operator", op, 201, "CW-20261016-0155"},
		{"device is dropped", dev, 201, ""},
		{"agent is refused", agent, 403, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &fakeLaunchService{}
			h := NewHandler(Deps{Service: svc})
			ctx := identity.WithPrincipal(context.Background(), tc.p)
			r := httptest.NewRequest("POST", "/sessions", strings.NewReader(`{"launch":"l1","task_id":"CW-20261016-0155"}`)).WithContext(ctx)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.code {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.code, w.Body.String())
			}
			if tc.code == 403 {
				if len(svc.createInputs) != 0 {
					t.Fatalf("a refused launch reached the service: %+v", svc.createInputs)
				}
				return
			}
			if tc.taskID == "" {
				// Dropped: the request is a plain launch, not a tier-2 create carrying a stamp.
				if len(svc.createInputs) != 0 || len(svc.createIDs) != 1 {
					t.Fatalf("dropped task id still reached the service: inputs=%+v ids=%v", svc.createInputs, svc.createIDs)
				}
				return
			}
			if len(svc.createInputs) != 1 || svc.createInputs[0].TaskID != tc.taskID {
				t.Fatalf("service input = %+v, want task %q", svc.createInputs, tc.taskID)
			}
		})
	}
}

// The real operator path: the tether CLI sends operator.token over the local API. Behind the actual identity
// middleware (enforcing), that bearer resolves to the operator credential, so POST /sessions carries the task id to
// the service and /sessions/bootstrap stores it; a request without the bearer never reaches the handlers at all.
func TestTaskStampThroughRealOperatorToken(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "op.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ids := identity.NewStore(db.DB())
	tokenPath := filepath.Join(t.TempDir(), "operator.token")
	if err := ids.EnsureOperator(context.Background(), tokenPath); err != nil {
		t.Fatal(err)
	}
	token, err := identity.ReadTokenFile(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	p, err := ids.Verify(context.Background(), token)
	if err != nil || !identity.IsOperatorCredential(p) {
		t.Fatalf("operator.token resolves to %+v err=%v, want the operator credential", p, err)
	}
	svc := &fakeLaunchService{}
	handler := identity.MiddlewareWithScopes(identity.Enforce, ids, nil, identity.ScopePolicy{}, NewHandler(Deps{Service: svc, SessionBootstrap: db}))
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	post := func(path, body string, bearer bool) int {
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if bearer {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	if got := post("/sessions", `{"launch":"l1","task_id":"CW-20261016-0155"}`, true); got != http.StatusCreated {
		t.Fatalf("POST /sessions with operator.token: %d", got)
	}
	if len(svc.createInputs) != 1 || svc.createInputs[0].TaskID != "CW-20261016-0155" {
		t.Fatalf("operator launch did not carry the task id: %+v", svc.createInputs)
	}
	if got := post("/sessions/bootstrap", `{"session_id":"op-s","task_id":"CW-20261016-0155"}`, true); got != http.StatusOK {
		t.Fatalf("bootstrap with operator.token: %d", got)
	}
	if row, err := db.GetSession("op-s"); err != nil || row.TaskID.String != "CW-20261016-0155" {
		t.Fatalf("bootstrap task = %+v err=%v", row.TaskID, err)
	}
	if got := post("/sessions", `{"launch":"l1","task_id":"CW-20261016-0155"}`, false); got != http.StatusUnauthorized {
		t.Fatalf("anonymous launch: %d, want 401 (it never reaches taskStamp)", got)
	}
}
