package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	messaging "github.com/hollis-labs/libs/mesh/core/messaging"
)

// CW-20261030-0177: ?summary=1 is a bounded, read-only backlog report. A stale
// pending delivery and a settled one are counted in their groups, with the
// recipient, and nothing about either delivery changes.
func TestMessageRetentionSummary_CountsGroupsAndChangesNothing(t *testing.T) {
	base, db := newRetentionTestServer(t)

	stale := sendAPIRetentionMessage(t, db)
	old := time.Now().UTC().Add(-20 * 24 * time.Hour).Format(time.RFC3339Nano)
	if _, err := db.DB().Exec(`UPDATE messaging_deliveries SET created_at = ? WHERE message_id = ?`, old, stale); err != nil {
		t.Fatal(err)
	}
	settled := sendAPIRetentionMessage(t, db)
	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
	if err := db.MessagingStore().Consume(context.Background(), settled, to); err != nil {
		t.Fatal(err)
	}
	ancient := time.Now().UTC().Add(-100 * 24 * time.Hour).Format(time.RFC3339Nano)
	if _, err := db.DB().Exec(`UPDATE messaging_deliveries SET updated_at = ? WHERE message_id = ?`, ancient, settled); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(base + "/messages/retention/candidates?summary=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Summary struct {
			ByStatus map[string]int `json:"by_status"`
			Groups   []struct {
				Name          string `json:"name"`
				Count         int    `json:"count"`
				TopRecipients []struct {
					Recipient string `json:"recipient"`
					Count     int    `json:"count"`
				} `json:"top_recipients"`
			} `json:"groups"`
		} `json:"summary"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, g := range out.Summary.Groups {
		counts[g.Name] = g.Count
		if g.Name == "stale_pending" && (len(g.TopRecipients) != 1 || g.TopRecipients[0].Count != 1) {
			t.Fatalf("stale_pending recipients = %+v", g.TopRecipients)
		}
	}
	if counts["stale_pending"] != 1 || counts["settled_over_90d"] != 1 || counts["dead_lettered_over_30d"] != 0 {
		t.Fatalf("groups = %v (by status %v)", counts, out.Summary.ByStatus)
	}
	if out.Summary.ByStatus["pending"] != 1 || out.Summary.ByStatus["delivered"] != 1 {
		t.Fatalf("by_status = %v, want one pending and one delivered", out.Summary.ByStatus)
	}
	// Report only: the stale delivery is still pending and its body is intact.
	var status string
	if err := db.DB().QueryRow(`SELECT status FROM messaging_deliveries WHERE message_id = ?`, stale).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("stale delivery status = %q (%v), want unchanged pending", status, err)
	}
}
