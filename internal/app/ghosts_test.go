package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workerhub"
	"github.com/hollis-labs/tether/internal/workerwire"
)

type ghostRig struct {
	t   *testing.T
	db  *store.Store
	svc *Service
	hub *workerhub.Hub
	url string
	ids *identity.Store
}

func newGhostRig(t *testing.T) *ghostRig {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	hub := workerhub.New(db.DB())
	t.Cleanup(hub.Close)
	ids := identity.NewStore(db.DB())
	remote := httptest.NewServer((&daemon.Server{Identity: ids, Workers: hub, Config: daemon.Config{RemoteListener: daemon.RemoteListenerConfig{AllowedHosts: []string{"worker.test"}}}}).RemoteHandler("worker.test"))
	t.Cleanup(remote.Close)
	return &ghostRig{t: t, db: db, svc: &Service{Store: db, Workers: hub}, hub: hub, url: remote.URL, ids: ids}
}

// connect enrolls a fake worker and sends its hello: the sessions it holds and, from a worker with
// CW-20261114-0056, its restore counts. It returns the worker id.
func (r *ghostRig) connect(label string, held []string, restore *workerwire.RestoreCounts) string {
	r.t.Helper()
	expires := time.Now().Add(time.Hour)
	token, err := r.ids.Mint(context.Background(), identity.Principal{ID: "msg://device/" + label, Kind: "device", Scopes: []string{identity.ScopeWorker}, ExpiresAt: &expires})
	if err != nil {
		r.t.Fatal(err)
	}
	header := http.Header{}
	header.Set("Host", "worker.test")
	header.Set("Authorization", "Bearer "+token)
	header.Set("Tether-Protocol", "1")
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(r.url, "http")+workerwire.ConnectPath, header)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = ws.Close() })
	var sessions []workerwire.Session
	for _, id := range held {
		sessions = append(sessions, workerwire.Session{ID: id, State: "running"})
	}
	if err = ws.WriteJSON(workerwire.Frame{T: "hello", Proto: 1, Worker: &workerwire.Worker{Label: label, Platform: "linux", Arch: "amd64"}, Sessions: sessions, Restore: restore}); err != nil {
		r.t.Fatal(err)
	}
	var welcome workerwire.Frame
	if err = ws.ReadJSON(&welcome); err != nil || welcome.T != "welcome" {
		r.t.Fatalf("welcome: %+v %v", welcome, err)
	}
	go func() {
		for {
			var f workerwire.Frame
			if ws.ReadJSON(&f) != nil {
				return
			}
		}
	}()
	id, err := r.hub.Resolve(context.Background(), label)
	if err != nil {
		r.t.Fatal(err)
	}
	return id
}

// session inserts a session row in state on worker, created age ago, with a live session credential
// and runtime binding when it is open.
func (r *ghostRig) session(id, worker, state string, age time.Duration) {
	r.t.Helper()
	row := store.SessionRow{ID: id, LaunchID: "l", ProjectID: "p", LogicalAgentID: "agent-" + id, ProviderID: "claude", ProviderKind: "cli", Workspace: r.t.TempDir(), State: "running", WorkerID: worker}
	if err := r.db.CreateSession(row, &launch.Plan{LaunchID: "l", ProjectID: "p", LogicalAgentID: row.LogicalAgentID, ProviderID: "claude"}); err != nil {
		r.t.Fatal(err)
	}
	created := time.Now().Add(-age).UTC().Format(time.RFC3339Nano)
	r.exec(`UPDATE sessions SET worker_id=?, created_at=?, updated_at=? WHERE id=?`, worker, created, created, id)
	if _, err := r.ids.Mint(context.Background(), identity.Principal{ID: "msg://session/local/" + id, Kind: "session", SessionID: id, Scopes: []string{"read"}}); err != nil {
		r.t.Fatal(err)
	}
	r.exec(`INSERT INTO runtime_bindings(id,target_urn,session_id,host_id,attempt_id,generation,visibility,leased_at,created_at,updated_at) VALUES(?,?,?,?,?,1,'tether-hosted',?,?,?)`, "b-"+id, "msg://agent/x/"+id, id, "h", "a", created, created, created)
	r.exec(`UPDATE sessions SET state=? WHERE id=?`, state, id)
}

func (r *ghostRig) shim(session string, pid int, socket string) {
	r.t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	r.exec(`INSERT INTO session_shims(session_id,shim_key,host_backend,socket_path,descriptor_path,runtime,runtime_generation,boot_generation,host_pid,created_at,updated_at) VALUES(?,?,'detached',?,'/d','claude','1','b',?,?,?)`, session, "k-"+session, socket, pid, now, now)
}

func (r *ghostRig) worker(id, label string, seen time.Duration) {
	r.t.Helper()
	at := time.Now().Add(-seen).UTC().Format(time.RFC3339Nano)
	r.exec(`INSERT INTO workers(id,label,device_id,platform,last_seen,created_at) VALUES(?,?,?,'linux/amd64',?,?)`, id, label, "dev-"+id, at, at)
}

func (r *ghostRig) exec(q string, args ...any) {
	r.t.Helper()
	if _, err := r.db.DB().Exec(q, args...); err != nil {
		r.t.Fatalf("%s: %v", q, err)
	}
}

func (r *ghostRig) scalar(q string, args ...any) string {
	r.t.Helper()
	var v string
	if err := r.db.DB().QueryRow(q, args...).Scan(&v); err != nil {
		r.t.Fatalf("%s: %v", q, err)
	}
	return v
}

func byClass(rep *GhostReport) map[string]GhostSession {
	out := map[string]GhostSession{}
	for _, g := range rep.Sessions {
		out[g.ID] = g
	}
	return out
}

// CW-20261114-0007: every ghost class is found against live state, nothing live is touched, a dry run
// changes nothing, and an apply needs a backup, closes ghosts with their credentials and bindings,
// removes dead custody and retires or deletes stale workers.
func TestReapGhosts(t *testing.T) {
	r := newGhostRig(t)
	ctx := context.Background()

	// A stale duplicate enrollment of "w1", seen 30 days ago, with ended history and a ghost.
	r.worker("worker_old_w1", "w1", 30*24*time.Hour)
	r.session("ended-on-old", "worker_old_w1", "completed", 40*24*time.Hour)
	r.session("ghost-gone", "worker_old_w1", "launching", 30*24*time.Hour)
	// A stale worker nothing references; one seen yesterday stays.
	r.worker("worker_unused", "laptop", 9*24*time.Hour)
	r.worker("worker_recent", "hub", 24*time.Hour)
	r.session("offline-recent", "worker_recent", "running", time.Hour)

	r.session("live", "", "running", time.Hour)
	r.session("ghost-missing", "", "running", time.Hour)
	r.session("fresh", "", "running", time.Minute)
	r.session("ghost-old-worker", "", "running", time.Hour)
	w1 := r.connect("w1", []string{"live", "fresh"}, &workerwire.RestoreCounts{Live: 2})
	old := r.connect("w-old", nil, nil) // predates restore counts: its inventory is not trusted
	r.exec(`UPDATE sessions SET worker_id=? WHERE id IN ('live','ghost-missing','fresh')`, w1)
	r.exec(`UPDATE sessions SET worker_id=? WHERE id='ghost-old-worker'`, old)

	r.session("hub-orphan", "", "orphaned", 30*24*time.Hour)
	r.shim("hub-orphan", 0, filepath.Join(t.TempDir(), "gone.sock"))
	r.session("hub-orphan-live-shim", "", "orphaned", 30*24*time.Hour)
	r.shim("hub-orphan-live-shim", os.Getpid(), "")
	r.session("killed-dead-shim", "", "killed", 30*24*time.Hour)
	r.shim("killed-dead-shim", 0, "")
	r.session("killed-live-shim", "", "killed", 30*24*time.Hour)
	r.shim("killed-live-shim", os.Getpid(), "")

	rep, err := r.svc.ReapGhosts(ctx, GhostReapOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := byClass(rep)
	want := map[string]string{"ghost-missing": "worker_missing", "ghost-gone": "worker_gone", "ghost-old-worker": "worker_missing", "hub-orphan": "hub_orphaned"}
	if len(got) != len(want) {
		t.Fatalf("ghosts = %+v", rep.Sessions)
	}
	for id, class := range want {
		if got[id].Class != class {
			t.Fatalf("%s: class %q, want %q", id, got[id].Class, class)
		}
	}
	if got["ghost-old-worker"].Action == "close" || !strings.HasPrefix(got["ghost-old-worker"].Action, "blocked:") {
		t.Fatalf("a worker without a proven inventory must block: %+v", got["ghost-old-worker"])
	}
	if got["ghost-missing"].Bindings != 1 || got["ghost-missing"].Action != "close" {
		t.Fatalf("%+v", got["ghost-missing"])
	}
	custody := map[string]string{}
	for _, c := range rep.Custody {
		custody[c.SessionID] = c.Action
	}
	if custody["killed-dead-shim"] != "delete" || !strings.HasPrefix(custody["killed-live-shim"], "keep:") || custody["hub-orphan"] != "delete after close" {
		t.Fatalf("custody = %v", custody)
	}
	workers := map[string]GhostWorker{}
	for _, w := range rep.Workers {
		workers[w.ID] = w
	}
	if w := workers["worker_old_w1"]; w.Action != "retire" || w.SupersededBy != w1 {
		t.Fatalf("old w1 = %+v (want retire, superseded by %s)", w, w1)
	}
	if w := workers["worker_unused"]; w.Action != "retire" || w.SupersededBy != "" {
		t.Fatalf("unused = %+v (stale workers are retired, never deleted)", w)
	}
	if _, ok := workers["worker_recent"]; ok {
		t.Fatal("a worker seen yesterday is in scope")
	}
	if rep.Before.GhostSessions != 4 || rep.Before.BlockedGhosts != 1 || rep.Before.DeadCustody != 2 || rep.Before.StaleWorkers != 2 || rep.After != nil {
		t.Fatalf("before = %+v after = %v", rep.Before, rep.After)
	}
	// The dry run changed nothing.
	if s := r.scalar(`SELECT state FROM sessions WHERE id='ghost-missing'`); s != "running" {
		t.Fatalf("dry run changed a session: %s", s)
	}
	if n := r.scalar(`SELECT COUNT(*) FROM session_shims`); n != "4" {
		t.Fatalf("dry run changed custody: %s", n)
	}

	// An apply needs a recent SQLite backup: none, a non-SQLite file and a stale one are bad requests.
	dir := t.TempDir()
	backup := filepath.Join(dir, "tether.db")
	if err := os.WriteFile(backup, []byte(sqliteHeader+"rest of the database"), 0o600); err != nil {
		t.Fatal(err)
	}
	notSQLite := filepath.Join(dir, "notes.txt")
	_ = os.WriteFile(notSQLite, []byte("a backup, honest"), 0o600)
	stale := filepath.Join(dir, "old.db")
	_ = os.WriteFile(stale, []byte(sqliteHeader), 0o600)
	_ = os.Chtimes(stale, time.Now().Add(-3*time.Hour), time.Now().Add(-3*time.Hour))
	for _, b := range []string{"", notSQLite, stale, filepath.Join(dir, "missing.db"), dir} {
		_, err = r.svc.ReapGhosts(ctx, GhostReapOptions{Apply: true, Backup: b})
		var bad interface{ BadRequest() bool }
		if !errors.As(err, &bad) || !errors.Is(err, ErrGhostBackupRequired) {
			t.Fatalf("apply with backup %q: %v", b, err)
		}
	}
	if s := r.scalar(`SELECT state FROM sessions WHERE id='ghost-missing'`); s != "running" {
		t.Fatal("a refused apply changed a session")
	}
	tokens := &recordingTokens{}
	r.svc.AgentTokens = tokens
	rep, err = r.svc.ReapGhosts(ctx, GhostReapOptions{Apply: true, Backup: backup})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"ghost-missing", "ghost-gone", "hub-orphan"} {
		if s := r.scalar(`SELECT state FROM sessions WHERE id=?`, id); s != "failed" {
			t.Fatalf("%s state %s", id, s)
		}
		if n := r.scalar(`SELECT COUNT(*) FROM principals WHERE session_id=? AND revoked_at IS NULL`, id); n != "0" {
			t.Fatalf("%s keeps a live credential", id)
		}
		if n := r.scalar(`SELECT COUNT(*) FROM runtime_bindings WHERE session_id=? AND revoked_at IS NULL`, id); n != "0" {
			t.Fatalf("%s keeps a live binding", id)
		}
	}
	if !sameSet(tokens.revoked, []string{"ghost-missing", "ghost-gone", "hub-orphan"}) {
		t.Fatalf("agent tokens revoked for %v", tokens.revoked)
	}
	for _, id := range []string{"live", "fresh", "ghost-old-worker", "offline-recent", "hub-orphan-live-shim"} {
		if s := r.scalar(`SELECT state FROM sessions WHERE id=?`, id); s == "failed" {
			t.Fatalf("%s was closed", id)
		}
	}
	if n := r.scalar(`SELECT group_concat(session_id) FROM session_shims ORDER BY session_id`); n != "hub-orphan-live-shim,killed-live-shim" {
		t.Fatalf("custody left: %s", n)
	}
	if r.scalar(`SELECT COALESCE(retired_at,'')||'|'||COALESCE(superseded_by,'') FROM workers WHERE id='worker_old_w1'`) == "|" {
		t.Fatal("old w1 not retired")
	}
	if n := r.scalar(`SELECT COUNT(*) FROM workers WHERE id='worker_unused' AND retired_at IS NOT NULL`); n != "1" {
		t.Fatal("unused worker not retired (or deleted)")
	}
	if id, _ := r.hub.Resolve(ctx, "w1"); id != w1 {
		t.Fatalf("label w1 resolves to %s", id)
	}
	list, _ := r.hub.List(ctx)
	for _, w := range list {
		if w.ID == "worker_old_w1" {
			t.Fatal("a retired worker is listed")
		}
	}
	if rep.After == nil || rep.After.GhostSessions != 1 || rep.After.BlockedGhosts != 1 || rep.After.DeadCustody != 0 || rep.After.StaleWorkers != 0 {
		t.Fatalf("after = %+v", rep.After)
	}
}

type recordingTokens struct{ revoked []string }

func (r *recordingTokens) RegisterSessionGrant(string, string, map[string][]string, time.Time) error {
	return nil
}
func (r *recordingTokens) RevokeSession(sid string) error {
	r.revoked = append(r.revoked, sid)
	return nil
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, x := range a {
		seen[x]++
	}
	for _, x := range b {
		if seen[x]--; seen[x] < 0 {
			return false
		}
	}
	return true
}
