package launchprofile

import (
	"testing"
	"time"
)

func durationText(s string) *string { return &s }

func TestLifecycleInheritanceAndExplicitDisable(t *testing.T) {
	parent := &LifecyclePolicy{IdleTimeout: durationText("30m"), MaxDuration: durationText("24h"), TerminateGrace: durationText("7s")}
	child := &LifecyclePolicy{MaxDuration: durationText("0s")}
	p := MergeLifecycle(parent, child)
	d, err := p.Durations()
	if err != nil {
		t.Fatal(err)
	}
	if d.IdleTimeout != 30*time.Minute || d.MaxDuration != 0 || d.TerminateGrace != 7*time.Second {
		t.Fatalf("resolved policy: %+v", d)
	}
	*p.IdleTimeout = "1s"
	if *parent.IdleTimeout != "30m" {
		t.Fatal("cascade mutated ancestor")
	}
}

func TestLifecycleNoImplicitCeilingAndInvalidLimits(t *testing.T) {
	var p *LifecyclePolicy
	d, err := p.Durations()
	if err != nil {
		t.Fatal(err)
	}
	if d.IdleTimeout != 0 || d.MaxDuration != 0 || d.LeaseDuration != 0 {
		t.Fatalf("implicit limit: %+v", d)
	}
	for _, s := range []string{"12hours", "-1s", ""} {
		if _, err := (&LifecyclePolicy{MaxDuration: durationText(s)}).Durations(); err == nil {
			t.Fatalf("accepted duration %q", s)
		}
	}
}
