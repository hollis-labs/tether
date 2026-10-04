package store

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/launch"
)

func TestSessionShimRoundTripAndIdentityFence(t *testing.T) {
	db, e := Open(filepath.Join(t.TempDir(), "shim.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = db.Close() }()
	if e = db.CreateSession(SessionRow{ID: "shim-session", State: "created"}, &launch.Plan{}); e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	row := SessionShimRow{SessionID: "shim-session", ShimKey: "host-key", HostBackend: "detached", SocketPath: "/private/control.sock", DescriptorPath: "/private/launch.json", Runtime: "claude-stream", RuntimeGeneration: 1, BootGeneration: "boot", JournalID: "journal", ControllerEpoch: math.MaxUint64, InjectCounter: math.MaxUint64, LastCommittedCursor: "journal:42", HostPID: 1, ShimPID: 1, ProviderPID: 2}
	if e = db.UpsertSessionShim(ctx, row); e != nil {
		t.Fatal(e)
	}
	got, e := db.SessionShim(ctx, row.SessionID)
	if e != nil {
		t.Fatal(e)
	}
	if got.InjectCounter != row.InjectCounter || got.ControllerEpoch != row.ControllerEpoch || got.LastCommittedCursor != row.LastCommittedCursor || got.DescriptorPath != row.DescriptorPath || got.ProviderPID != row.ProviderPID || got.CreatedAt == "" {
		t.Fatalf("round trip: %+v", got)
	}
	created := got.CreatedAt
	row.ProviderPID = 3
	row.CreatedAt = "ignored"
	if e = db.UpsertSessionShim(ctx, row); e != nil {
		t.Fatal(e)
	}
	got, e = db.SessionShim(ctx, row.SessionID)
	if e != nil || got.ProviderPID != 3 || got.CreatedAt != created {
		t.Fatalf("update: %+v %v", got, e)
	}
	row.JournalID = "other"
	if e = db.UpsertSessionShim(ctx, row); !errors.Is(e, ErrSessionShimConflict) {
		t.Fatalf("journal replacement: %v", e)
	}
	rows, e := db.ListSessionShims(ctx)
	if e != nil || len(rows) != 1 || rows[0].ShimKey != "host-key" {
		t.Fatalf("list: %+v %v", rows, e)
	}
	row.SessionID = "absent"
	if e = db.UpsertSessionShim(ctx, row); !errors.Is(e, ErrSessionNotFound) {
		t.Fatalf("absent session: %v", e)
	}
}
