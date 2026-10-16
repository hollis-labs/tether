package eventretention

import (
	"strings"
	"testing"
	"time"
)

func mustConfig(t *testing.T, body string) Config {
	t.Helper()
	c, _, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func mustPolicy(t *testing.T, c Config, class Class) Policy {
	t.Helper()
	p, err := c.Snapshot(class)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func TestDefaultsAndForwardPolicyEpoch(t *testing.T) {
	defaults := mustConfig(t, "")
	for _, tc := range []struct {
		class          Class
		full, envelope Age
		rollups        []RollupRule
	}{
		{Audit, 0, -1, []RollupRule{{Age(Day), 0}}},
		{State, Age(180 * Day), Age(2 * Year), []RollupRule{{Age(time.Hour), Age(2 * Year)}, {Age(Day), 0}}},
		{Activity, Age(30 * Day), Age(180 * Day), []RollupRule{{Age(time.Hour), Age(2 * Year)}, {Age(Day), 0}}},
		{Debug, Age(7 * Day), -1, []RollupRule{{Age(time.Hour), Age(90 * Day)}}},
	} {
		p := mustPolicy(t, defaults, tc.class)
		r := p.Rule()
		if r.Full != tc.full || r.Envelope != tc.envelope || len(r.Rollups) != len(tc.rollups) {
			t.Fatalf("%s defaults: %+v", tc.class, r)
		}
		for i, want := range tc.rollups {
			if r.Rollups[i] != want {
				t.Fatalf("%s rollup: %+v", tc.class, r)
			}
		}
	}
	old := mustPolicy(t, defaults, State)
	changed := mustConfig(t, "retention:\n  classes:\n    state:\n      full: 1d\n")
	newer := mustPolicy(t, changed, State)
	if old.Epoch() == newer.Epoch() || old.Rule().Full != Age(180*Day) || newer.Rule().Full != Age(Day) {
		t.Fatal("policy update changed old snapshot")
	}
	if changed.ID() != mustConfig(t, "retention: {classes: {state: {full: 24h}}}").ID() {
		t.Fatal("equivalent configs differ")
	}
	copyRule := old.Rule()
	copyRule.Rollups[0].Keep = 1
	if old.Rule().Rollups[0].Keep != Age(2*Year) {
		t.Fatal("snapshot was mutable")
	}
}
func TestConfigValidation(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"unknown", "retention: {surprise: true}", "surprise"},
		{"unknown class", "retention: {classes: {bogus: {full: 1d}}}", "bogus"},
		{"secret", "retention: {classes: {secret: {full: 0}}}", "secret"},
		{"secret knob", "retention: {store_secrets: true}", "store_secrets"},
		{"negative", "retention: {pii_cap: -1d}", "duration"},
		{"overflow", "retention: {pii_cap: 999999999999999999999y}", "duration"},
		{"float", "retention: {pii_cap: 1.5}", "duration"},
		{"full none", "retention: {classes: {state: {full: none}}}", "tier"},
		{"tier order", "retention: {classes: {state: {full: 90d, envelope: 30d}}}", "precedes"},
		{"duplicate bucket", "retention: {classes: {state: {rollups: [{bucket: 1h, keep: 1d}, {bucket: 1h, keep: 0}]}}}", "duplicate"},
		{"zero bucket", "retention: {classes: {state: {rollups: [{bucket: 0, keep: 1d}]}}}", "bucket"},
		{"empty rollups", "retention: {classes: {state: {rollups: []}}}", "rollup"},
		{"multi document", "retention: {}\n---\nretention: {}", "one YAML"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := Parse([]byte(tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v want %s", err, tc.want)
			}
		})
	}
}
func TestPIIExtensionAndFiniteAuditNeedAcknowledgment(t *testing.T) {
	for _, body := range []string{
		"retention: {pii_cap: 31d}",
		"retention: {pii_cap: 0}",
		"retention: {classes: {audit: {full: 400d}}}",
		"retention: {classes: {audit: {full: 800d}}}",
	} {
		_, warnings, err := Parse([]byte(body))
		if err == nil || len(warnings) != 1 {
			t.Fatalf("risk accepted without ack: %v %v", err, warnings)
		}
		body = strings.Replace(body, "retention: {", "retention: {acknowledge_risk: true, ", 1)
		_, warnings, err = Parse([]byte(body))
		if err != nil || len(warnings) != 1 {
			t.Fatalf("ack: %v %v", err, warnings)
		}
	}
	for _, body := range []string{
		"retention: {pii_cap: 1d}",
		"retention: {classes: {state: {rollups: [{bucket: 1d, keep: 1h}]}}}",
		"retention: {classes: {state: {full: 1h, envelope: none, rollups: [{bucket: 1h, keep: 1h}]}}}",
	} {
		_, warnings, err := Parse([]byte(body))
		if err != nil || len(warnings) != 0 {
			t.Fatalf("free shortening rejected: %v %v", err, warnings)
		}
	}
}

func TestAuditPayloadFloorCannotBeAcknowledgedAway(t *testing.T) {
	for _, body := range []string{
		"retention: {classes: {audit: {full: 399d}}}",
		"retention: {acknowledge_risk: true, classes: {audit: {full: 399d}}}",
		"retention: {acknowledge_risk: true, classes: {audit: {full: 1d, envelope: 400d}}}",
	} {
		if _, _, err := Parse([]byte(body)); err == nil || !strings.Contains(err.Error(), "400d") {
			t.Fatalf("audit payload floor bypassed: %v", err)
		}
	}
	c := mustConfig(t, "retention: {acknowledge_risk: true, pii_cap: 1d, classes: {audit: {full: 400d}}}")
	r := Record{Cursor: 1, RecordedAt: time.Now(), Policy: mustPolicy(t, c, Audit), PII: true, PayloadPresent: true, RolledUp: true}
	if d, err := Plan(r, r.RecordedAt.Add(2*Day)); err != nil || !d.Tombstone || d.TrimEligible {
		t.Fatalf("PII audit override %+v %v", d, err)
	}
}
