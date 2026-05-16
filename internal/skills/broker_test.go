package skills

import (
	"path/filepath"
	"testing"
)

func TestBrokerLayered_RanksQueryAndTriggers(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	mustWriteSkill(t, root, "refactor-go", `---
id: refactor-go
name: Refactor Go
description: Apply Go refactoring patterns.
triggers: [refactor, backend]
priority: 10
---

Prefer small, tested changes.
`)
	mustWriteSkill(t, root, "capture-notes", `---
id: capture-notes
name: Capture Notes
description: Capture investigation notes.
triggers: [capture, backend]
priority: 50
---

Write down findings.
`)

	got, err := BrokerLayered(root, root, BrokerQuery{
		Query:    "refactor handler",
		Role:     "backend",
		Triggers: []string{"refactor"},
		Limit:    2,
	})
	if err != nil {
		t.Fatalf("BrokerLayered: %v", err)
	}
	if len(got.Matches) != 2 {
		t.Fatalf("len(matches) = %d; want 2", len(got.Matches))
	}
	if got.Matches[0].Skill.ID != "refactor-go" {
		t.Fatalf("top match = %q; want refactor-go", got.Matches[0].Skill.ID)
	}
	if got.Matches[0].Score.QueryMatches == 0 {
		t.Fatalf("top match query score = 0; want > 0")
	}
	if got.Matches[0].Score.PreferredMatches == 0 {
		t.Fatalf("top match preferred score = 0; want > 0")
	}
}

func TestBrokerLayered_FiltersByLayer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()

	mustWriteSkill(t, root, "system-skill", `---
id: system-skill
name: System Skill
description: System catalog.
triggers: [backend]
---

System.
`)
	path := filepath.Join(home, ".tether", "skills", "user-skill.md")
	if err := writeFileEnsureDir(path, `---
id: user-skill
name: User Skill
description: User catalog.
triggers: [backend]
---

User.
`); err != nil {
		t.Fatal(err)
	}

	got, err := BrokerLayered(root, root, BrokerQuery{
		Role:   "backend",
		Layers: []string{"user"},
		Limit:  5,
	})
	if err != nil {
		t.Fatalf("BrokerLayered: %v", err)
	}
	if len(got.Matches) != 1 {
		t.Fatalf("len(matches) = %d; want 1", len(got.Matches))
	}
	if got.Matches[0].Skill.ID != "user-skill" {
		t.Fatalf("match = %q; want user-skill", got.Matches[0].Skill.ID)
	}
}

func mustWriteSkill(t *testing.T, root, id, body string) {
	t.Helper()
	if err := writeFileEnsureDir(filepath.Join(root, "skills", id+".md"), body); err != nil {
		t.Fatal(err)
	}
}
