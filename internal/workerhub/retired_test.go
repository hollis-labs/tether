package workerhub

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/store"
)

// CW-20261114-0007: a label shared by a stale enrollment and a live one resolves to the newest unretired
// row (by id the stale one sorted first, so "home-laptop" resolved to a dead worker), and a retired row is
// hidden from listings and label lookups but still resolves by its exact id.
func TestResolveSkipsRetiredAndPrefersTheNewestEnrollment(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err = db.DB().ExecContext(ctx, `INSERT INTO workers(id,label,device_id,platform,caps_json,last_seen,created_at) VALUES
		('worker_000stale','home','device_old','darwin','{}','2026-10-16T21:30:34Z','2026-10-16T21:15:19Z'),
		('worker_fffnew','home','device_new','darwin','{}','2026-11-14T18:00:00Z','2026-10-16T21:32:31Z')`); err != nil {
		t.Fatal(err)
	}
	h := New(db.DB())
	if id, _ := h.Resolve(ctx, "home"); id != "worker_fffnew" {
		t.Fatalf("label resolved to %s, want the newest enrollment", id)
	}
	if _, err = db.DB().ExecContext(ctx, `UPDATE workers SET retired_at='2026-11-14T19:00:00Z', superseded_by='worker_fffnew' WHERE id='worker_000stale'`); err != nil {
		t.Fatal(err)
	}
	if id, _ := h.Resolve(ctx, "worker_000stale"); id != "worker_000stale" {
		t.Fatalf("a retired worker no longer resolves by id: %q", id)
	}
	list, err := h.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != "worker_fffnew" {
		t.Fatalf("list = %+v", list)
	}
}
