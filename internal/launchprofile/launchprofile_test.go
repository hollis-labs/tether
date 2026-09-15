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

func TestLaunchProfile(t *testing.T) {
	lp := launchprofile.LaunchProfile{
		ID:   "test-profile",
		Name: "Test Profile",
	}
	if lp.ID != "test-profile" || lp.Name != "Test Profile" {
		t.Errorf("expected ID and Name to match, got ID=%q Name=%q", lp.ID, lp.Name)
	}

	a := launchprofile.Agent{
		ID:   "test-agent",
		Name: "Test Agent",
	}
	if a.ID != "test-agent" || a.Name != "Test Agent" {
		t.Errorf("expected ID and Name to match, got ID=%q Name=%q", a.ID, a.Name)
	}
}

func TestLaunchContext(t *testing.T) {
	lc := launchprofile.LaunchContext{
		ID:       "test-context",
		Name:     "Test Context",
		RepoRoot: "/repo/root",
	}
	if lc.ID != "test-context" || lc.Name != "Test Context" || lc.RepoRoot != "/repo/root" {
		t.Errorf("expected fields to match, got ID=%q Name=%q RepoRoot=%q", lc.ID, lc.Name, lc.RepoRoot)
	}

	p := launchprofile.Project{
		ID:       "test-project",
		Name:     "Test Project",
		RepoRoot: "/project/root",
	}
	if p.ID != "test-project" || p.Name != "Test Project" || p.RepoRoot != "/project/root" {
		t.Errorf("expected fields to match, got ID=%q Name=%q RepoRoot=%q", p.ID, p.Name, p.RepoRoot)
	}
}
