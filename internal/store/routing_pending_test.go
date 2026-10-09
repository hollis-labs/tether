package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	messaging "github.com/hollis-labs/substrate/mesh/messaging"
	"github.com/hollis-labs/tether/internal/store"
)

func TestPendingTurnOutputsPagesByStagingTimeThenID(t *testing.T) {
	db := openRetentionDB(t)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Minute)
	// Descending IDs ensure ID-only ordering cannot accidentally pass. Include
	// equal timestamps and legacy fractional forms that need zero padding.
	offsets := []time.Duration{0, 100 * time.Millisecond, 100 * time.Millisecond, 100100 * time.Microsecond, 200 * time.Millisecond}
	ids := []string{"z", "y", "x", "w", "v"}
	for i, id := range ids {
		env, err := db.StageTurnOutput(ctx, messaging.Envelope{From: messaging.Address{Kind: messaging.KindSession, Authority: "local", ID: "s1"}, Payload: []byte(`{"text":"answer"}`), Metadata: map[string]string{"turn_id": fmt.Sprint(i), "kind": "final"}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.DB().Exec(`UPDATE messages SET id=?,created_at=? WHERE id=?`, id, base.Add(offsets[i]).Format(time.RFC3339Nano), env.ID); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"z", "x", "y", "w", "v"}
	var cursor store.PendingTurnOutputCursor
	var got []string
	for {
		page, err := db.PendingTurnOutputs(ctx, cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		for _, item := range page {
			got = append(got, item.MessageID)
			cursor = store.PendingTurnOutputCursor{CreatedAt: item.CreatedAt, MessageID: item.MessageID}
		}
		// The last item is no longer pending once attached; the cursor must not
		// depend on looking up a queue row that may vanish between pages.
		if _, err := db.DB().Exec(`UPDATE messages SET routing_staged=0 WHERE id=?`, cursor.MessageID); err != nil {
			t.Fatal(err)
		}
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("recovery order: got %v want %v", got, want)
	}
}
