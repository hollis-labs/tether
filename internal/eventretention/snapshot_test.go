package eventretention

import (
	"strings"
	"testing"
)

func TestSnapshotRoundTripDoesNotApplyNewDefaults(t *testing.T) {
	c := mustConfig(t, "retention: {acknowledge_risk: true, pii_cap: 60d, classes: {audit: {full: 400d}, state: {full: 2h}}}")
	encoded, err := c.EncodeSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreSnapshot(encoded)
	if err != nil || restored.ID() != c.ID() {
		t.Fatalf("snapshot: %s %v", restored.ID(), err)
	}
	if err := restored.ValidateForPass(); err == nil {
		t.Fatal("restored audit evidence accepted as current configuration")
	}
	if restored.piiCap != c.piiCap || restored.rules[State].Full != c.rules[State].Full {
		t.Fatal("snapshot applied defaults")
	}
	for _, raw := range []string{`{}`, string(encoded) + `{}`, strings.Replace(string(encoded), `"version":1`, `"version":2`, 1), strings.Replace(string(encoded), `"bucket":3600000000000`, `"bucket":0`, 1), strings.Replace(string(encoded), `"debug"`, `"unknown"`, 1)} {
		if _, err := RestoreSnapshot([]byte(raw)); err == nil {
			t.Fatalf("invalid snapshot accepted: %s", raw)
		}
	}
}

func TestHistoricalAuditSnapshotIsEvidenceNotCurrentPolicy(t *testing.T) {
	c := mustConfig(t, "retention: {acknowledge_risk: true, classes: {audit: {full: 400d}}}")
	encoded, err := c.EncodeSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	legacy := strings.Replace(string(encoded), `"full":34560000000000000`, `"full":86400000000000`, 1)
	restored, err := RestoreSnapshot([]byte(legacy))
	if err != nil {
		t.Fatal(err)
	}
	if restored.rules[Audit].Full != Age(Day) || restored.ID() == c.ID() {
		t.Fatal("historical audit snapshot reinterpreted")
	}
	if err := restored.ValidateForPass(); err == nil {
		t.Fatal("historical below-floor snapshot accepted as current policy")
	}
	if err := c.ValidateForPass(); err != nil {
		t.Fatal(err)
	}
}
