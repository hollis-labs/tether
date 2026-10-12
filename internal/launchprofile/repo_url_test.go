package launchprofile

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestProjectRepoURLDecodes(t *testing.T) {
	var with Project
	if err := yaml.Unmarshal([]byte("id: tether\nname: Tether\nrepo_root: ~/dev/tether\nrepo_url: https://github.com/hollis-labs/tether.git\n"), &with); err != nil {
		t.Fatal(err)
	}
	if with.RepoURL != "https://github.com/hollis-labs/tether.git" {
		t.Fatalf("RepoURL = %q", with.RepoURL)
	}
	// Existing project files do not declare it and must keep loading unchanged.
	var without Project
	if err := yaml.Unmarshal([]byte("id: old\nname: Old\nrepo_root: ~/dev/old\n"), &without); err != nil {
		t.Fatal(err)
	}
	if without.RepoURL != "" {
		t.Fatalf("RepoURL = %q, want empty", without.RepoURL)
	}
}
