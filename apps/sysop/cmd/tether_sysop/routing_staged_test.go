package main

import (
	"context"
	"path/filepath"
	"testing"

	messaging "github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/tether/internal/store"
)

func TestSysopStagedOutputHiddenFromUserInboxAndOverview(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	env, err := db.StageTurnOutput(context.Background(), messaging.Envelope{
		From: messaging.Address{Kind: messaging.KindSession, Authority: "local", ID: "s1"}, Payload: []byte(`{"text":"hidden"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	user := messaging.Address{Kind: messaging.KindUser, Authority: "local", ID: "chris"}
	if _, err := db.DB().Exec(`UPDATE messages SET to_urn=? WHERE id=?`, user.URN(), env.ID); err != nil {
		t.Fatal(err)
	}
	totals, err := countMessages(db)
	if err != nil || totals.Total != 0 {
		t.Fatalf("User totals exposed stage: %+v %v", totals, err)
	}
	rows, err := db.ListMessages(100)
	if err != nil || len(rows) != 0 {
		t.Fatalf("User list exposed stage: %+v %v", rows, err)
	}
	var overview overviewResponse
	populateOverviewMessages(db, &overview)
	if overview.Messages.Total != 0 {
		t.Fatalf("overview exposed stage: %+v", overview.Messages)
	}
	// Ordinary User messages still count and list as before.
	if _, err := db.MessagingStore().Send(context.Background(), messaging.Envelope{Kind: messaging.MsgKindNotice, From: env.From, To: user, Payload: []byte(`{"text":"visible"}`)}); err != nil {
		t.Fatal(err)
	}
	totals, err = countMessages(db)
	if err != nil || totals.Total != 1 {
		t.Fatalf("ordinary User message missing: %+v %v", totals, err)
	}
}
