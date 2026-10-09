package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTurnOutputRetryJournalUsesSelectedStateAndSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "selected", "state.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("a", 64)
	if err := db.WriteTurnOutputRetry(id, []byte(`{"fixture":"operational output"}`)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(path+".turn-output-retries", id+".json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("retry body is not private", err)
	}
	info, err = os.Stat(path + ".turn-output-retries")
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatal("retry directory is not private", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	payload, err := db.ReadTurnOutputRetry(id)
	if err != nil || string(payload) != `{"fixture":"operational output"}` {
		t.Fatal("journal lost after reopen", err)
	}
	if err := db.CompleteTurnOutputRetry(id); err != nil {
		t.Fatal(err)
	}
	if ids, err := db.PendingTurnOutputRetries("", 128); err != nil || len(ids) != 0 {
		t.Fatal("completed record replayed", err)
	}
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if err := ro.WriteTurnOutputRetry(id, payload); err == nil {
		t.Fatal("read-only client wrote daemon journal")
	}
}

func TestTurnOutputRetryJournalRejectsInvalidPathsAndExcludesExpiredRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, id := range []string{"../escape", strings.Repeat("/", 64), strings.Repeat("A", 64)} {
		if err := db.WriteTurnOutputRetry(id, []byte(`{}`)); err == nil {
			t.Fatal("invalid journal path accepted")
		}
	}
	first, second := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for _, id := range []string{first, second} {
		if err := db.WriteTurnOutputRetry(id, []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := db.PendingTurnOutputRetries(first, 1)
	if err != nil || len(ids) != 1 || ids[0] != second {
		t.Fatal("journal cursor lost pending output", err)
	}
	old := time.Now().Add(-RoutingStageRetention - time.Hour)
	if err := os.Chtimes(filepath.Join(path+".turn-output-retries", first+".json"), old, old); err != nil {
		t.Fatal(err)
	}
	ids, err = db.PendingTurnOutputRetries("", 128)
	if err != nil || len(ids) != 1 || ids[0] != second {
		t.Fatal("expired journal scheduled for replay", err)
	}
	if _, err := db.ReadTurnOutputRetry(first); err != nil {
		t.Fatal("retention implicitly purged output", err)
	}
	if published, err := db.HasPublishedTurnOutput(context.Background(), "none", second); err != nil || published {
		t.Fatal("invented published output", err)
	}
}
