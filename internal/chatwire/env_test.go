package chatwire

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/libs/agent/sdk/chathost"
	"github.com/hollis-labs/libs/mesh/core/runtimecontract"

	"github.com/hollis-labs/tether/internal/conversation"
	"github.com/hollis-labs/tether/internal/launch"
	tstore "github.com/hollis-labs/tether/internal/store"
)

// env is a Tether store, a chat database and a chat host in a temp directory.
type env struct {
	dir      string
	st       *tstore.Store
	db       *DB
	host     *chathost.Host
	resolver conversation.Resolver
	ctl      *Control
}

func allowAll(context.Context, runtimecontract.OwnerID, chathost.Permission) error { return nil }

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	e := &env{dir: t.TempDir()}
	st, err := tstore.Open(filepath.Join(e.dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	e.st, e.resolver = st, conversation.Resolver{Store: st}
	if e.db, err = OpenDB(ctx, filepath.Join(e.dir, "chat", "chat.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.db.Close() })
	if e.host, err = chathost.Open(filepath.Join(e.dir, "chathost"), allowAll); err != nil {
		t.Fatal(err)
	}
	e.ctl, err = NewControl(ControlConfig{
		DB: e.db.Store, Host: e.host,
		Seat: func(ctx context.Context, agent string) (string, error) {
			t, err := e.resolver.BySeat(ctx, agent)
			return t.ConversationID, err
		},
		Target: e.resolver.ByConversation,
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// session writes a session for seat; parent makes it a resume, which bumps the seat's conversation.
func (e *env) session(t testing.TB, id, seat, parent string) {
	t.Helper()
	row := tstore.SessionRow{ID: id, LaunchID: "l", ProjectID: "p", LogicalAgentID: seat, ProviderID: "pv", Workspace: "/tmp/ws", State: "running"}
	if parent != "" {
		row.Intent, row.ParentSessionID = "resume", sql.NullString{String: parent, Valid: true}
	}
	if err := e.st.CreateSession(row, &launch.Plan{LaunchID: "l"}); err != nil {
		t.Fatalf("create session %s: %v", id, err)
	}
}

// relaunch writes a resume session for seat's current session and returns the new session's ID.
func (e *env) relaunch(t testing.TB, seat string) string {
	t.Helper()
	cur, err := e.resolver.BySeat(context.Background(), seat)
	if err != nil {
		t.Fatalf("seat %s: %v", seat, err)
	}
	id := cur.SessionID + "r"
	e.session(t, id, seat, cur.SessionID)
	return id
}
