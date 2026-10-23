package launchprofile

import (
	"strings"
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

func TestLifecycleStopAfterFinalTurnIsOptInAndCascades(t *testing.T) {
	var none *LifecyclePolicy
	if d, _ := none.Durations(); d.StopAfterFinalTurn != 0 {
		t.Fatalf("default must be disabled: %v", d.StopAfterFinalTurn)
	}
	p := MergeLifecycle(&LifecyclePolicy{StopAfterFinalTurn: durationText("45s")}, &LifecyclePolicy{IdleTimeout: durationText("10m")})
	d, err := p.Durations()
	if err != nil || d.StopAfterFinalTurn != 45*time.Second {
		t.Fatalf("got %+v err=%v", d, err)
	}
	if _, err := (&LifecyclePolicy{StopAfterFinalTurn: durationText("-1s")}).Durations(); err == nil {
		t.Fatal("negative grace accepted")
	}
}

func TestLifecycleBindingMode(t *testing.T) {
	str := func(s string) *string { return &s }
	if got := (*LifecyclePolicy)(nil).BindingMode(); got != BindingReplace {
		t.Fatalf("nil policy mode = %q", got)
	}
	for _, mode := range []string{BindingReplace, BindingKeep, BindingNone} {
		if _, err := (&LifecyclePolicy{Binding: str(mode)}).Durations(); err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
	}
	if _, err := (&LifecyclePolicy{Binding: str("steal")}).Durations(); err == nil || !strings.Contains(err.Error(), "lifecycle.binding") {
		t.Fatalf("an unknown mode was accepted: %v", err)
	}
	// The launch overrides the agent; unset inherits.
	merged := MergeLifecycle(&LifecyclePolicy{Binding: str(BindingNone)}, &LifecyclePolicy{IdleTimeout: str("1m")})
	if merged.BindingMode() != BindingNone {
		t.Fatalf("inherited mode = %q, want none", merged.BindingMode())
	}
	merged = MergeLifecycle(&LifecyclePolicy{Binding: str(BindingNone)}, &LifecyclePolicy{Binding: str(BindingKeep)})
	if merged.BindingMode() != BindingKeep {
		t.Fatalf("overridden mode = %q, want keep", merged.BindingMode())
	}
}
