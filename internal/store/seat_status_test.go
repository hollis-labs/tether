package store

import (
	"context"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/seatstatus"
)

func TestSeatStatusPutGetAndUnchangedResetKeepsSince(t *testing.T) {
	ctx := context.Background()
	db := openEventsStore(t)
	urn := "msg://agent/agent-mux/agt_lpy0gym4eh"
	if _, found, err := db.GetSeatStatus(ctx, urn); err != nil || found {
		t.Fatalf("empty get: found=%v err=%v", found, err)
	}
	t0 := time.Date(2026, 11, 14, 4, 0, 0, 0, time.UTC)
	d := seatstatus.Declared{AgentURN: urn, State: seatstatus.Working, Reason: "0013", Since: t0, SetBy: urn, UpdatedAt: t0, ExpiresAt: t0.Add(time.Hour)}
	if _, changed, err := db.PutSeatStatus(ctx, d); err != nil || !changed {
		t.Fatalf("first put: changed=%v err=%v", changed, err)
	}
	t1 := t0.Add(10 * time.Minute)
	again := d
	again.Since, again.UpdatedAt, again.ExpiresAt = t1, t1, t1.Add(time.Hour)
	stored, changed, err := db.PutSeatStatus(ctx, again)
	if err != nil || changed || !stored.Since.Equal(t0) {
		t.Fatalf("re-set: changed=%v since=%v err=%v", changed, stored.Since, err)
	}
	got, found, err := db.GetSeatStatus(ctx, urn)
	if err != nil || !found || !got.Since.Equal(t0) || !got.ExpiresAt.Equal(t1.Add(time.Hour)) || got.State != seatstatus.Working {
		t.Fatalf("get = %+v found=%v err=%v", got, found, err)
	}
	blocked := d
	blocked.State, blocked.Owner, blocked.Since = seatstatus.Blocked, "@archie", t1
	if _, changed, err := db.PutSeatStatus(ctx, blocked); err != nil || !changed {
		t.Fatalf("blocked: changed=%v err=%v", changed, err)
	}
	blocked.Owner = ""
	if _, _, err := db.PutSeatStatus(ctx, blocked); err == nil {
		t.Fatal("blocked without owner stored")
	}
}
