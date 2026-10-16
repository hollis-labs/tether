package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

func TestIdentityUnboundedCountsAndListsWithoutChangingAnything(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ids := identity.NewStore(db.DB())
	future := time.Now().Add(time.Hour)
	for id, p := range map[string]identity.Principal{
		"s-anon":   {CreatedBy: "", LaunchAttribution: "anonymous_external"},
		"s-legacy": {CreatedBy: ""},
		"s-ok":     {CreatedBy: identity.OperatorID, LaunchAttribution: "principal"},
	} {
		if err = db.CreateSession(store.SessionRow{ID: id, State: "created"}, nil); err != nil {
			t.Fatal(err)
		}
		p.ID, p.Kind, p.SessionID, p.ExpiresAt = "msg://session/local/"+id, "session", id, &future
		if _, err = ids.MintSessionForLaunch(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	list, err := reportUnbounded(context.Background(), &out, ids)
	if err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"2 live unbounded", "anonymous_external", "legacy_unknown_launcher", "s-anon", "s-legacy"} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "s-ok") || len(list) != 2 {
		t.Errorf("a bounded credential was listed:\n%s", text)
	}
	// Read-only: running it again lists the same two.
	again, err := ids.UnboundedSessions(context.Background())
	if err != nil || len(again) != 2 {
		t.Fatalf("report changed state: %v %v", again, err)
	}
}
