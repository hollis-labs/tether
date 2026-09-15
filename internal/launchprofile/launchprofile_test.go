package launchprofile_test

import (
	"testing"

	"github.com/hollis-labs/tether/internal/launchprofile"
)

func TestPlanEffectiveWorkRoot(t *testing.T) {
	var nilPlan *launchprofile.Plan
	if got := nilPlan.EffectiveWorkRoot(); got != "" {
		t.Errorf("nil Plan: expected empty string, got %q", got)
	}

	p := &launchprofile.Plan{
		RepoRoot: "/path/to/repo",
	}
	if got := p.EffectiveWorkRoot(); got != "/path/to/repo" {
		t.Errorf("Plan with RepoRoot: expected /path/to/repo, got %q", got)
	}

	p.WorkRoot = "/path/to/work"
	if got := p.EffectiveWorkRoot(); got != "/path/to/work" {
		t.Errorf("Plan with WorkRoot: expected /path/to/work, got %q", got)
	}
}
