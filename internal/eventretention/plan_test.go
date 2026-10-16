package eventretention

import (
	"crypto/sha256"
	"fmt"
	"testing"
	"time"
)

func TestPlanTiersPIIAndHeadBarriers(t *testing.T) {
	c := mustConfig(t, "")
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	base := Record{Cursor: 1, RecordedAt: at, Policy: mustPolicy(t, c, Activity), PayloadPresent: true, RolledUp: true}
	for _, tc := range []struct {
		name       string
		age        time.Duration
		change     func(*Record)
		tomb, trim bool
	}{
		{"full", 29 * Day, nil, false, false},
		{"envelope", 30 * Day, nil, true, false},
		{"rollup", 180 * Day, nil, true, true},
		{"pending", 200 * Day, func(r *Record) { r.Pending = true }, false, false},
		{"unrolled", 200 * Day, func(r *Record) { r.RolledUp = false }, true, false},
		{"legacy", 200 * Day, func(r *Record) { r.Policy = Policy{} }, false, false},
		{"future", -Day, nil, false, false},
		{"audit", 800 * Day, func(r *Record) { r.Policy = mustPolicy(t, c, Audit) }, false, false},
		{"pii audit", 30 * Day, func(r *Record) { r.Policy = mustPolicy(t, c, Audit); r.PII = true }, true, false},
		{"no payload", 200 * Day, func(r *Record) { r.PayloadPresent = false }, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := base
			if tc.change != nil {
				tc.change(&r)
			}
			got, err := Plan(r, at.Add(tc.age))
			if err != nil || got.Tombstone != tc.tomb || got.TrimEligible != tc.trim {
				t.Fatalf("%+v %v", got, err)
			}
		})
	}
	for _, barrier := range []func(*Record){func(r *Record) { r.Pending = true }, func(r *Record) { r.RolledUp = false }, func(r *Record) { r.Policy = mustPolicy(t, c, Audit) }, func(r *Record) { r.Policy = Policy{} }, func(r *Record) { r.RecordedAt = at.Add(200 * Day) }} {
		rows := []Record{base, base, base}
		rows[1].Cursor = 7
		rows[2].Cursor = 10
		barrier(&rows[1])
		through, n, err := HeadTrim(rows, at.Add(200*Day), 100)
		if err != nil || through != 1 || n != 1 {
			t.Fatalf("skipped barrier: %d %d %v", through, n, err)
		}
	}
	rows := []Record{base, base}
	rows[1].Cursor = 7
	through, n, err := HeadTrim(rows, at.Add(200*Day), 1)
	if err != nil || through != 1 || n != 1 {
		t.Fatalf("unbounded trim: %d %d %v", through, n, err)
	}
	rows[1].Cursor = 1
	if _, _, err := HeadTrim(rows, at.Add(200*Day), 10); err == nil {
		t.Fatal("unsorted input accepted")
	}
	base.Secret = true
	if _, err := Plan(base, at); err == nil {
		t.Fatal("secret admitted")
	}
}
func TestTombstoneExactBytesAndPIISentinel(t *testing.T) {
	a := []byte(`{"a":1}`)
	b := []byte(`{ "a": 1 }`)
	payload, digest := Tombstone(a, false)
	if payload != nil || digest != fmt.Sprintf("sha256:%x", sha256.Sum256(a)) {
		t.Fatalf("digest %s", digest)
	}
	_, other := Tombstone(b, false)
	if other == digest {
		t.Fatal("payload re-canonicalized")
	}
	_, pii := Tombstone(a, true)
	if pii != "withheld" {
		t.Fatal("PII digest leaks hash or loses tombstone marker")
	}
	_, absent := Tombstone(nil, true)
	if absent != "" {
		t.Fatal("absent payload marked as removed")
	}
	_, empty := Tombstone([]byte{}, false)
	if empty == "" {
		t.Fatal("empty present payload treated as absent")
	}
}
func TestRollupCheckpointBucketsAndRetention(t *testing.T) {
	c := mustConfig(t, "")
	at := time.Date(2026, 1, 1, 10, 5, 0, 0, time.UTC)
	p := mustPolicy(t, c, State)
	rows := []Record{{Cursor: 2, RecordedAt: at, Policy: p}, {Cursor: 3, RecordedAt: at.Add(time.Minute), Policy: p}, {Cursor: 7, RecordedAt: at.Add(2 * time.Hour), Policy: p}}
	sums, through, err := RollupBatch(rows, 0)
	if err != nil || through != 7 || len(sums) != 3 {
		t.Fatalf("%v %d %v", sums, through, err)
	}
	for _, sum := range sums {
		if sum.Bucket == Day && (sum.Count != 3 || sum.FirstCursor != 2 || sum.LastCursor != 7) {
			t.Fatalf("daily %+v", sum)
		}
		if sum.Bucket == time.Hour && sum.Start.Hour() == 10 && (sum.Count != 2 || sum.FirstCursor != 2 || sum.LastCursor != 3) {
			t.Fatalf("hourly %+v", sum)
		}
		if sum.Bucket == Day && sum.Expired(at.Add(100*Year)) {
			t.Fatal("forever rollup expired")
		}
		if sum.Bucket == time.Hour && (!sum.Expired(sum.Start.Add(time.Hour+2*Year)) || sum.Expired(sum.Start.Add(time.Hour+2*Year-time.Nanosecond))) {
			t.Fatal("rollup expiry boundary")
		}
	}
	replay, last, err := RollupBatch(rows, through)
	if err != nil || len(replay) != 0 || last != through {
		t.Fatal("checkpoint replay double counted")
	}
	rows[1].Pending = true
	sums, through, err = RollupBatch(rows, 0)
	if err != nil || through != 2 {
		t.Fatalf("pending row skipped: %d %v", through, err)
	}
	for _, sum := range sums {
		if sum.Count != 1 {
			t.Fatal("counted past pending barrier")
		}
	}
	rows[1].Pending = false
	rows[1].PII = true
	newer := mustConfig(t, "retention: {classes: {state: {rollups: [{bucket: 1h, keep: 1d}]}}}")
	rows[2].Policy = mustPolicy(t, newer, State)
	sums, _, err = RollupBatch(rows, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, sum := range sums {
		if sum.Count != 1 {
			t.Fatal("sensitivity/epoch buckets collapsed")
		}
	}
	rows[2].Secret = true
	if sums, _, err = RollupBatch(rows, 0); err == nil || sums != nil {
		t.Fatal("partial batch emitted before invalid secret")
	}
}
