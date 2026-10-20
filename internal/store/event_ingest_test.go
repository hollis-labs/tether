package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestIngestDigestRoundTripAndRetention(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "digest.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	if _, found, err := db.IngestDigest(ctx, "evt_1"); err != nil || found {
		t.Fatalf("empty store: %v %v", found, err)
	}
	old := time.Now().Add(-100 * 24 * time.Hour)
	if err := db.RecordIngestDigest(ctx, "evt_1", "sha256:a", "torque", old); err != nil {
		t.Fatal(err)
	}
	// The first digest wins; a repeat does not replace it.
	if err := db.RecordIngestDigest(ctx, "evt_1", "sha256:b", "torque", old); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordIngestDigest(ctx, "evt_2", "sha256:c", "torque", time.Now()); err != nil {
		t.Fatal(err)
	}
	if d, found, _ := db.IngestDigest(ctx, "evt_1"); !found || d != "sha256:a" {
		t.Fatalf("digest = %q %v", d, found)
	}
	batch, err := db.DeleteEventHistoryBefore(ctx, "event_ingest_digests", time.Now().Add(-90*24*time.Hour), 100)
	if err != nil || batch.Removed != 1 {
		t.Fatalf("retention removed %d: %v", batch.Removed, err)
	}
	if _, found, _ := db.IngestDigest(ctx, "evt_1"); found {
		t.Fatal("old digest survived retention")
	}
	if _, found, _ := db.IngestDigest(ctx, "evt_2"); !found {
		t.Fatal("recent digest was removed")
	}
	if _, found, err := db.EventByEventID(ctx, "evt_missing"); err != nil || found {
		t.Fatalf("missing event: %v %v", found, err)
	}
}
