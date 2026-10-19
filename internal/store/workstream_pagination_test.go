package store

import (
	"errors"
	"fmt"
	"testing"
)

func TestWorkstreamPagination(t *testing.T) {
	db := openTestStoreForSessions(t)
	for i := 0; i < 107; i++ {
		if _, err := db.CreateWorkstream(WorkstreamRow{ID: fmt.Sprintf("w%03d", i), WorkflowID: "wf", Status: "active"}); err != nil {
			t.Fatal(err)
		}
	}
	// Force identical timestamps so every boundary exercises the ID tiebreaker.
	if _, err := db.DB().Exec(`UPDATE workstreams SET created_at='2026-10-19T00:00:00Z'`); err != nil {
		t.Fatal(err)
	}
	opts := ListWorkstreamsOptions{WorkflowID: "wf", Status: "active", Limit: 20}
	seen := map[string]bool{}
	pages := 0
	for {
		page, err := db.ListWorkstreamsPage(opts)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, row := range page.Workstreams {
			if seen[row.ID] {
				t.Fatal("duplicate", row.ID)
			}
			seen[row.ID] = true
		}
		if !page.HasMore {
			if page.NextCursor != "" {
				t.Fatal("final page cursor")
			}
			break
		}
		if pages == 1 {
			if page.Workstreams[0].ID != "w106" || page.Workstreams[19].ID != "w087" {
				t.Fatalf("unstable ordering: %+v", page.Workstreams)
			}
			for _, bad := range []ListWorkstreamsOptions{{Cursor: page.NextCursor}, {Status: "closed", WorkflowID: "wf", Cursor: page.NextCursor}, {Status: "active", WorkflowID: "other", Cursor: page.NextCursor}} {
				if _, err := db.ListWorkstreamsPage(bad); !errors.Is(err, ErrWorkstreamPage) {
					t.Fatalf("cross-filter cursor accepted: %v", err)
				}
			}
			if _, err := db.CreateWorkstream(WorkstreamRow{ID: "new-head", WorkflowID: "wf"}); err != nil {
				t.Fatal(err)
			}
		}
		opts.Cursor = page.NextCursor
	}
	if len(seen) != 107 || pages != 6 {
		t.Fatalf("seen=%d pages=%d", len(seen), pages)
	}
}

func TestWorkstreamPageBounds(t *testing.T) {
	db := openTestStoreForSessions(t)
	for _, id := range []string{"a", "b"} {
		if _, err := db.CreateWorkstream(WorkstreamRow{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := db.ListWorkstreamsPage(ListWorkstreamsOptions{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if page.HasMore || page.NextCursor != "" || len(page.Workstreams) != 2 {
		t.Fatalf("exact boundary: %+v", page)
	}
	empty, err := db.ListWorkstreamsPage(ListWorkstreamsOptions{WorkflowID: "none"})
	if err != nil || empty.HasMore || len(empty.Workstreams) != 0 {
		t.Fatalf("empty: %+v %v", empty, err)
	}
	clamped, err := db.ListWorkstreamsPage(ListWorkstreamsOptions{Limit: 1000000})
	if err != nil || clamped.Limit != MaxWorkstreamLimit {
		t.Fatalf("clamp: %+v %v", clamped, err)
	}
	for _, bad := range []ListWorkstreamsOptions{{Limit: -1}, {Cursor: "bad"}, {Status: "invalid"}, {Ref: "bad"}, {Ref: "task:x", Status: "active"}} {
		if _, err := db.ListWorkstreamsPage(bad); !errors.Is(err, ErrWorkstreamPage) {
			t.Fatalf("bad page accepted: %+v %v", bad, err)
		}
	}
}
