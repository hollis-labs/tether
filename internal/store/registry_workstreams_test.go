package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/hollis-labs/tether/internal/launch"
)

func TestRegistryWorkstreamReferencesAndTaskNoMint(t *testing.T) {
	db := openWorkstreamStore(t)
	const id = "workstream:01M7VQ2QWYB5RZR86W2BWG8PW3"
	mustCreateSession(t, db, SessionRow{ID: "parent", State: "created", TaskID: parentRef("task"), WorkstreamID: parentRef(id)})
	for _, intent := range []string{"compact", "resume", "fork"} {
		row := mustCreateSession(t, db, SessionRow{ID: intent, ParentSessionID: parentRef("parent"), Intent: intent})
		if row.WorkstreamID.String != id {
			t.Fatal("external identity not inherited")
		}
		w, err := db.EnsureSessionWorkstream(row.ID, WorkstreamRow{})
		if err != nil || w.ID != id {
			t.Fatal("external reference unavailable", err)
		}
	}
	if err := db.CreateSession(SessionRow{ID: "unbound", State: "created", TaskID: parentRef("task")}, &launch.Plan{}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.EnsureSessionWorkstream("unbound", WorkstreamRow{ID: id}); !errors.Is(err, ErrRegistryWorkstreamRequired) {
		t.Fatal("task minted local identity", err)
	}
	if err := db.FillSessionLaunchCorrelation(context.Background(), "unbound", "task", id); err != nil {
		t.Fatal(err)
	}
	w, err := db.GetWorkstream(id)
	if err != nil || w.ID != id {
		t.Fatal("launch fill lost reference", err)
	}
	var count int
	if err := db.DB().QueryRow(`SELECT count(*) FROM workstreams`).Scan(&count); err != nil || count != 1 {
		t.Fatal("duplicate/local workstream minted")
	}
	// Legacy dangling references remain errors rather than being relabeled.
	mustCreateSession(t, db, SessionRow{ID: "legacy", WorkstreamID: sql.NullString{String: "old-label", Valid: true}})
	if _, err := db.EnsureSessionWorkstream("legacy", WorkstreamRow{}); err == nil {
		t.Fatal("legacy label treated as Registry ID")
	}
}

func TestPreviouslyAdmittedRegistryReferenceIsRetainedOnLaunch(t *testing.T) {
	db := openWorkstreamStore(t)
	const id = "workstream:01M7VQ2QWYB5RZR86W2BWG8PW3"
	mustCreateSession(t, db, SessionRow{ID: "pre-existing", State: "created", WorkstreamID: parentRef(id)})
	if _, err := db.DB().Exec(`DELETE FROM workstreams WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if err := db.FillSessionLaunchCorrelation(context.Background(), "pre-existing", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetWorkstream(id); err != nil {
		t.Fatal("external reference not retained", err)
	}
}
