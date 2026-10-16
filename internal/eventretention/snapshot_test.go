package eventretention

import (
	"strings"
	"testing"
)

func TestSnapshotRoundTripDoesNotApplyNewDefaults(t *testing.T) {
	c := mustConfig(t, "retention: {acknowledge_risk: true, pii_cap: 60d, classes: {audit: {full: 1d}, state: {full: 2h}}}")
	encoded, err := c.EncodeSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreSnapshot(encoded)
	if err != nil || restored.ID() != c.ID() {
		t.Fatalf("snapshot: %s %v", restored.ID(), err)
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
