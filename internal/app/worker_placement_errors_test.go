package app

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workerhub"
)

// A launch worker label that is not enrolled, or no channel at all, must fail
// with classifiable errors that name the label, never a bare string.
func TestCreateWorkerSessionPlacementErrors(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	svc := &Service{Store: db, Workers: workerhub.New(db.DB())}
	_, err = svc.CreateSessionWithInput(CreateSessionInput{LaunchID: "pm", Worker: "ghost"})
	if !errors.Is(err, workerhub.ErrNotEnrolled) || !strings.Contains(err.Error(), `"ghost"`) {
		t.Fatalf("unenrolled worker err = %v", err)
	}

	_, err = (&Service{Store: db}).CreateSessionWithInput(CreateSessionInput{LaunchID: "pm", Worker: "ghost"})
	if !errors.Is(err, workerhub.ErrChannelUnavailable) {
		t.Fatalf("no channel err = %v", err)
	}
}
