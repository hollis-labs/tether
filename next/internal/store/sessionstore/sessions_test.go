package sessionstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hollis-labs/tether/next/internal/store"
	"github.com/hollis-labs/tether/next/internal/store/sessionstore"
	"github.com/hollis-labs/tether/next/internal/store/storetest"
)

var t0 = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

func newStore(s *store.Stores) (*sessionstore.Store, *storetest.Clock) {
	ss := sessionstore.New(s)
	clock := storetest.NewClock(t0)
	ss.Now = clock.Now
	return ss, clock
}

func row(id string) sessionstore.SessionRow {
	return sessionstore.SessionRow{ID: id, LaunchID: "launch", ProjectID: "proj", LogicalAgentID: "agent", ProviderID: "claude", ProviderKind: "cli", Workspace: "/w", State: "launching"}
}

func TestSessionsCore(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ss, clock := newStore(s)
		parent := row("ses-parent")
		parent.App, parent.RouteJSON = "tangent", `{"channel":"news"}`
		if _, err := ss.CreateSession(ctx, parent, `{"plan":1}`, nil); err != nil {
			t.Fatal(err)
		}
		clock.Advance(time.Second)
		child := row("ses-child")
		child.ParentSessionID, child.App = "ses-parent", "ignored"
		key := &sessionstore.SessionIdempotency{PrincipalID: "msg://agent/x/y", Key: "k1", Operation: sessionstore.IdempotencyOpCreate, RequestDigest: "d1"}
		created, err := ss.CreateSession(ctx, child, "", key)
		if err != nil || created.App != "tangent" || created.Intent != "fresh" || created.Publication != "private-local" {
			t.Fatalf("child %+v %v", created, err)
		}
		if _, err := ss.CreateSession(ctx, row("ses-legacy"), "", &sessionstore.SessionIdempotency{PrincipalID: sessionstore.IdempotencyPrincipalLegacy}); err == nil {
			t.Fatal("legacy principal claimed a key")
		}
		got, err := ss.GetSession(ctx, "ses-child")
		if err != nil || got.ParentSessionID != "ses-parent" || got.App != "tangent" || got.PID != nil || got.CreatedAt != t0.Add(time.Second).Format(time.RFC3339) {
			t.Fatalf("get %+v %v", got, err)
		}
		if plan, _ := ss.LaunchPlanJSON(ctx, "ses-child"); plan != "null" {
			t.Fatalf("plan %q", plan)
		}
		if _, err := ss.CreateSession(ctx, row("ses-dup"), "", key); !errors.Is(err, sessionstore.ErrIdempotencyKeyTaken) {
			t.Fatalf("second claim of a key: %v", err)
		}
		if _, err := ss.GetSession(ctx, "ses-dup"); !errors.Is(err, sessionstore.ErrSessionNotFound) {
			t.Fatalf("a refused create wrote its session: %v", err)
		}
		if rec, _ := ss.GetSessionIdempotency(ctx, "msg://agent/x/y", "k1"); rec == nil || rec.SessionID != "ses-child" {
			t.Fatalf("idempotency %+v", rec)
		}
		if has, _ := ss.SessionHasIdempotencyKey(ctx, "ses-child"); !has {
			t.Fatal("key not found by session")
		}
		if err := ss.ReleaseSessionIdempotency(ctx, "msg://agent/x/y", "k1"); err != nil {
			t.Fatal(err)
		}
		if rec, _ := ss.GetSessionIdempotency(ctx, "msg://agent/x/y", "k1"); rec != nil {
			t.Fatal("released key still found")
		}
		exit := 3
		if err := ss.UpdateSessionState(ctx, "ses-child", "running", 42, nil); err != nil {
			t.Fatal(err)
		}
		if err := ss.SetSessionProcessStart(ctx, "ses-child", 42, "t-start"); err != nil {
			t.Fatal(err)
		}
		if err := ss.UpdateSessionState(ctx, "ses-child", "failed", 42, &exit); err != nil {
			t.Fatal(err)
		}
		got, _ = ss.GetSession(ctx, "ses-child")
		if got.State != "failed" || got.PID == nil || *got.PID != 42 || got.ExitCode == nil || *got.ExitCode != 3 || got.EndedAt == "" {
			t.Fatalf("state %+v", got)
		}
		if _, err := ss.GetSession(ctx, "nope"); !errors.Is(err, sessionstore.ErrSessionNotFound) {
			t.Fatalf("missing: %v", err)
		}
		list, _ := ss.ListSessions(ctx, sessionstore.ListSessionsOptions{})
		if len(list) != 2 || list[0].ID != "ses-child" {
			t.Fatalf("list %+v", list)
		}
		page, _ := ss.ListSessions(ctx, sessionstore.ListSessionsOptions{Cursor: sessionstore.SessionCursor(list[0].CreatedAt, list[0].ID)})
		if len(page) != 1 || page[0].ID != "ses-parent" {
			t.Fatalf("cursor page %+v", page)
		}
		if f, _ := ss.ListSessions(ctx, sessionstore.ListSessionsOptions{State: "launching"}); len(f) != 1 || f[0].ID != "ses-parent" {
			t.Fatalf("state filter %+v", f)
		}
		if _, err := ss.ListSessions(ctx, sessionstore.ListSessionsOptions{Cursor: "garbage"}); err == nil {
			t.Fatal("bad cursor accepted")
		}
	})
}
