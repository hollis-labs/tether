package mcptransport

import (
	"context"
	"testing"

	"github.com/hollis-labs/tether/internal/store"
)

type capturedRefStore struct{ rows []store.SessionRefRow }

func (s *capturedRefStore) AttachSessionRef(row store.SessionRefRow) (store.AttachRefResult, error) {
	s.rows = append(s.rows, row)
	return store.AttachRefResult{Inserted: true}, nil
}

func TestHubRefAttacherPinsSessionAndSource(t *testing.T) {
	s := &capturedRefStore{}
	sink := hubRefAttacher{sessionID: "verified-session", store: s}
	for _, id := range []string{"", "forged-session"} {
		if err := sink.AttachSessionRef(context.Background(), id, "torque_task", "CW-20261016-0157", "uri", "read", "proxy", "parent"); err == nil {
			t.Fatal("accepted mismatched session")
		}
	}
	if len(s.rows) != 0 {
		t.Fatal("mismatched session reached store")
	}
	if err := sink.AttachSessionRef(context.Background(), "verified-session", "torque_task", "CW-20261016-0157", "uri", "read", "operator", "parent"); err != nil {
		t.Fatal(err)
	}
	if len(s.rows) != 1 || s.rows[0].SessionID != "verified-session" || s.rows[0].Source != store.SourceProxy || s.rows[0].ParentItemID != "parent" {
		t.Fatalf("capture = %+v", s.rows)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sink.AttachSessionRef(ctx, "verified-session", "torque_task", "CW-20261016-0158", "", "read", "proxy", ""); err == nil {
		t.Fatal("accepted canceled call")
	}
	if len(s.rows) != 1 {
		t.Fatal("canceled call reached store")
	}
}
