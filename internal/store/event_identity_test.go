package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

const frozenTestEnvironment = "f64b485f-7f8d-441b-89d3-378b70e81352"

func insertLegacyIDFixture(t *testing.T, s *Store, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := s.db.Exec(`INSERT INTO events(scope,at,kind) VALUES('daemon','2026-01-01T00:00:00Z','legacy')`); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFrozenEventIDsResumeWithoutChangingExistingIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	insertLegacyIDFixture(t, db, 5)
	early := "evt_0123456789abcdef0123456789abcdef"
	if _, err := db.db.Exec(`UPDATE events SET event_id=? WHERE id=3`, early); err != nil {
		t.Fatal(err)
	}
	seeded, err := db.SeedEventIdentity(ctx, frozenTestEnvironment)
	if err != nil || seeded.Cutover != 5 || seeded.Remaining != 4 {
		t.Fatalf("cutover excludes or overwrites legacy rows: %+v %v", seeded, err)
	}
	_, _, newID, err := db.InsertEventIDContext(ctx, "daemon", "", "new", "")
	if err != nil || !strings.HasPrefix(newID, "evt_") {
		t.Fatalf("new event not minted: %s %v", newID, err)
	}
	part, err := db.BackfillEventIDs(ctx, 2)
	if err != nil || part.NextLegacyID != 3 || part.Remaining != 2 {
		t.Fatalf("chunk boundary: %+v %v", part, err)
	}
	var newest, oldest string
	if err := db.db.QueryRow(`SELECT event_id FROM events WHERE id=5`).Scan(&newest); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow(`SELECT COALESCE(event_id,'') FROM events WHERE id=1`).Scan(&oldest); err != nil {
		t.Fatal(err)
	}
	if newest != frozenTestEnvironment+":5" || oldest != "" {
		t.Fatal("backfill did not start at the newest legacy rows")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := db.SeedEventIdentity(ctx, frozenTestEnvironment)
	if err != nil || resumed != part {
		t.Fatalf("restart reset cutover/progress: %+v %v", resumed, err)
	}
	if _, err := db.SeedEventIdentity(ctx, "38fd4cce-66a0-4d28-b09e-2387fb6b577d"); !errors.Is(err, ErrEventIdentityMismatch) {
		t.Fatalf("identity mismatch accepted: %v", err)
	}
	for i := 0; i < 3; i++ {
		resumed, err = db.BackfillEventIDs(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
	}
	if resumed.Remaining != 0 || resumed.Backfilled != 4 || resumed.Cutover != 5 || resumed.Dropped != 0 {
		t.Fatalf("completion: %+v", resumed)
	}
	got, err := db.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range got {
		want := fmt.Sprintf("%s:%d", frozenTestEnvironment, e.Seq)
		if e.Seq == 3 {
			want = early
		}
		if e.Seq == 6 {
			want = newID
		}
		if e.EventID != want {
			t.Fatalf("row%d ID=%q want%q", e.Seq, e.EventID, want)
		}
	}
	again, err := db.BackfillEventIDs(ctx, 1)
	if err != nil || again != resumed {
		t.Fatalf("completed run changed: %+v %v", again, err)
	}
}

func TestFrozenEventIDChunkFailureIsAtomicAndResumable(t *testing.T) {
	db := openEventsStore(t)
	ctx := context.Background()
	insertLegacyIDFixture(t, db, 2)
	before, err := db.SeedEventIdentity(ctx, frozenTestEnvironment)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`CREATE TRIGGER fail_event_id BEFORE UPDATE OF event_id ON events WHEN NEW.id=1 BEGIN SELECT RAISE(ABORT,'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.BackfillEventIDs(ctx, 2); err == nil {
		t.Fatal("injected chunk failure ignored")
	}
	after, err := db.EventIdentityProgress(ctx)
	if err != nil || after != before {
		t.Fatalf("failed chunk advanced progress: %+v %v", after, err)
	}
	got, err := db.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range got {
		if e.EventID != "" {
			t.Fatal("failed chunk committed a partial ID update")
		}
	}
	if err := db.RecordEventIDBackfillFailure(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`DROP TRIGGER fail_event_id`); err != nil {
		t.Fatal(err)
	}
	after, err = db.BackfillEventIDs(ctx, 2)
	if err != nil || after.Remaining != 0 || after.Failed != 1 || after.Backfilled != 2 || after.Dropped != 0 {
		t.Fatalf("recovery: %+v %v", after, err)
	}
}

func TestFrozenEventIDRequiresSeedAndValidIdentity(t *testing.T) {
	db := openEventsStore(t)
	ctx := context.Background()
	if _, err := db.BackfillEventIDs(ctx, 10); !errors.Is(err, ErrEventIdentityUnseeded) {
		t.Fatalf("unseeded backfill: %v", err)
	}
	if err := db.RecordEventIDBackfillFailure(ctx); !errors.Is(err, ErrEventIdentityUnseeded) {
		t.Fatalf("unseeded failure counter: %v", err)
	}
	for _, id := range []string{"", "../other", "not-a-uuid", "00000000-0000-0000-0000-000000000000"} {
		if _, err := db.SeedEventIdentity(ctx, id); err == nil {
			t.Fatalf("accepted environment%q", id)
		}
	}
}
