package store

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/launch"
)

func mkConversationSession(t *testing.T, db *Store, id, agentID, parent string, plan *launch.Plan) {
	t.Helper()
	row := SessionRow{ID: id, LaunchID: "l", ProjectID: "p", LogicalAgentID: agentID, ProviderID: "pv", Workspace: "/tmp/ws", State: "created"}
	if parent != "" {
		row.Intent, row.ParentSessionID = "resume", sql.NullString{String: parent, Valid: true}
	}
	if plan == nil {
		plan = &launch.Plan{LaunchID: "l"}
	}
	if err := db.CreateSession(row, plan); err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
}

func conversationReasons(t *testing.T, db *Store, id string) []string {
	t.Helper()
	rows, err := db.db.Query(`SELECT generation, session_id, reason FROM conversation_generations WHERE conversation_id=? ORDER BY generation`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var g int64
		var s, r string
		if err := rows.Scan(&g, &s, &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, s+":"+r)
	}
	return out
}

var ulidPattern = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

func TestConversationFollowsSeatAcrossCreateLaunchResume(t *testing.T) {
	ctx := context.Background()
	db := openTestStoreForSessions(t)
	mkConversationSession(t, db, "s1", "seat", "", nil)
	c, held, err := db.ConversationForSession(ctx, "s1")
	if err != nil || held != 1 || c.Generation != 1 || c.SessionID != "s1" || c.Seat != "seat" || c.Agent != "seat" || c.Owner != ConversationOwnerOperator || c.Kind != ConversationKindStable {
		t.Fatalf("first = %+v held=%d err=%v", c, held, err)
	}
	if !ulidPattern.MatchString(c.ID) || strings.Contains(c.ID, "s1") || !ulidPattern.MatchString(c.AttemptID) || c.AttemptID == c.ID {
		t.Fatalf("ids not opaque ULIDs: conversation=%q attempt=%q", c.ID, c.AttemptID)
	}
	// A fresh launch for the same seat reuses the row and moves it.
	mkConversationSession(t, db, "s2", "seat", "", nil)
	// A relaunch/resume moves it again.
	mkConversationSession(t, db, "s3", "seat", "s2", nil)
	got, err := db.Conversation(ctx, c.ID)
	if err != nil || got.Generation != 3 || got.SessionID != "s3" || got.CreatedAt != c.CreatedAt {
		t.Fatalf("after resume = %+v err=%v", got, err)
	}
	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM conversations`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("conversations = %d, %v; want exactly one for the seat", n, err)
	}
	if r := conversationReasons(t, db, c.ID); strings.Join(r, ",") != "s1:create,s2:launch,s3:resume" {
		t.Fatalf("generations = %v", r)
	}
	// An older session still resolves, and reports the generation it held.
	old, held, err := db.ConversationForSession(ctx, "s1")
	if err != nil || held != 1 || old.SessionID != "s3" || old.Generation != 3 {
		t.Fatalf("superseded lookup = %+v held=%d err=%v", old, held, err)
	}
	// A retried write of the current session never bumps twice.
	tx, err := db.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := recordConversationTx(ctx, tx, "s3", "", ConversationReasonResume); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if again, _ := db.Conversation(ctx, c.ID); again.Generation != 3 {
		t.Fatalf("retry bumped generation to %d", again.Generation)
	}
}

func TestConversationSeatsAreSeparate(t *testing.T) {
	ctx := context.Background()
	db := openTestStoreForSessions(t)
	mkConversationSession(t, db, "adhoc", "", "", nil)
	if _, _, err := db.ConversationForSession(ctx, "adhoc"); !errors.Is(err, ErrConversationNotFound) {
		t.Fatalf("session without a seat got a conversation: %v", err)
	}
	// Team members keep the catalog agent as the operative and their own seat.
	mkConversationSession(t, db, "m1", "coder#aaaa", "", &launch.Plan{LaunchID: "l", CatalogAgentID: "coder", TeamMember: true})
	mkConversationSession(t, db, "m2", "coder#bbbb", "", &launch.Plan{LaunchID: "l", CatalogAgentID: "coder", TeamMember: true})
	a, _, errA := db.ConversationForSession(ctx, "m1")
	b, _, errB := db.ConversationForSession(ctx, "m2")
	if errA != nil || errB != nil || a.ID == b.ID || a.Agent != "coder" || a.Seat != "coder#aaaa" || b.Generation != 1 {
		t.Fatalf("team seats a=%+v b=%+v errs=%v %v", a, b, errA, errB)
	}
	seat, err := db.SeatConversation(ctx, ConversationOwnerOperator, "coder", "coder#bbbb")
	if err != nil || seat.ID != b.ID {
		t.Fatalf("seat lookup = %+v %v", seat, err)
	}
}

// The conversation write is part of the session transaction: if it fails, the
// session row is not persisted either.
func TestConversationWriteSharesSessionTransaction(t *testing.T) {
	db := openTestStoreForSessions(t)
	if _, err := db.db.Exec(`CREATE TRIGGER fail_conversation BEFORE INSERT ON conversation_generations BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	row := SessionRow{ID: "s1", LaunchID: "l", ProjectID: "p", LogicalAgentID: "seat", ProviderID: "pv", Workspace: "/tmp/ws", State: "created"}
	if err := db.CreateSession(row, &launch.Plan{LaunchID: "l"}); err == nil {
		t.Fatal("create succeeded with a failing conversation write")
	}
	if _, err := db.GetSession("s1"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("session persisted without its conversation: %v", err)
	}
	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM conversations`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("conversations = %d, %v", n, err)
	}
}

func TestConversationMigrationIsIdempotent(t *testing.T) {
	db := openTestStoreForSessions(t)
	mkConversationSession(t, db, "s1", "seat", "", nil)
	res, err := Migrate(db.db)
	if err != nil || len(res.Applied) != 0 {
		t.Fatalf("re-migrate applied %v, %v", res.Applied, err)
	}
	raw, err := embeddedMigrations.ReadFile("migrations/0107_conversations.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(string(raw)); err != nil {
		t.Fatalf("re-running 0107 by hand: %v", err)
	}
	if _, _, err := db.ConversationForSession(context.Background(), "s1"); err != nil {
		t.Fatalf("existing row lost: %v", err)
	}
}

func TestNewULIDIsTimeOrderedAndWellFormed(t *testing.T) {
	t0 := time.UnixMilli(1_700_000_000_000)
	a, b := newULID(t0), newULID(t0.Add(time.Millisecond))
	if !ulidPattern.MatchString(a) || !ulidPattern.MatchString(b) || a[:10] >= b[:10] {
		t.Fatalf("ulids %q %q", a, b)
	}
	// Known encoding of the timestamp part (ULID spec: 10 chars of 48-bit ms).
	if got := newULID(time.UnixMilli(1469918176385))[:10]; got != "01ARYZ6S41" {
		t.Fatalf("timestamp encoding = %q", got)
	}
}
