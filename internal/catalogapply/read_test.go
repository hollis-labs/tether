package catalogapply

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadAndListFiles(t *testing.T) {
	e := newEngine(t)
	write(t, e.Root, "providers/claude.yaml", "id: claude\n")
	write(t, e.Root, "agents/leaky.yaml", "id: leaky\napi_key: sk-abcdefghijklmnopqrstuvwxyz0123\n")

	got, err := e.ReadFile("agents/general.yaml")
	if err != nil || got != "id: general\nname: General\n" {
		t.Fatalf("read = %q, %v", got, err)
	}
	for name, path := range map[string]string{
		"provider":    "providers/claude.yaml",
		"global":      "global.yaml",
		"credentials": "credentials/token.txt",
		"traversal":   "agents/../global.yaml",
		"secret":      "agents/leaky.yaml",
		"missing":     "agents/none.yaml",
	} {
		if _, err := e.ReadFile(path); err == nil {
			t.Errorf("%s: read accepted", name)
		}
	}
	if err := os.Symlink(filepath.Join(e.Root, "global.yaml"), filepath.Join(e.Root, "agents", "link.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ReadFile("agents/link.yaml"); err == nil {
		t.Error("symlinked file read accepted")
	}

	files, err := e.ListFiles()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range files {
		names = append(names, f.Path)
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "agents/general.yaml") || !strings.Contains(joined, "launches/general.yaml") {
		t.Fatalf("list = %v", names)
	}
	for _, bad := range []string{"providers/", "global.yaml", "credentials/"} {
		if strings.Contains(joined, bad) {
			t.Errorf("list exposes %s: %v", bad, names)
		}
	}
}

func TestHistoryRecordsWhoPlannedAndApproved(t *testing.T) {
	e := newEngine(t)
	ctx := context.Background()
	if recs, err := e.History(10); err != nil || len(recs) != 0 {
		t.Fatalf("empty history = %v, %v", recs, err)
	}
	caller := Caller{Principal: "session:s1", SessionID: "s1", AgentID: "manager"}
	for _, name := range []string{"first", "second"} {
		p, err := e.PlanBundle(ctx, Bundle{Name: name, TaskID: "CW-1", Files: []File{{Path: "agents/" + name + ".yaml", Content: "id: " + name + "\n"}}}, Options{Caller: caller})
		if err != nil {
			t.Fatal(err)
		}
		if p.PlannedBy != caller {
			t.Fatalf("plan lost its caller: %+v", p.PlannedBy)
		}
		approve(t, e, p)
		if _, err := e.Apply(ctx, p.ID); err != nil {
			t.Fatal(err)
		}
	}
	recs, err := e.History(10)
	if err != nil || len(recs) != 2 {
		t.Fatalf("history = %v, %v", recs, err)
	}
	if recs[0].Name != "second" || recs[1].Name != "first" {
		t.Fatalf("not newest first: %v %v", recs[0].Name, recs[1].Name)
	}
	r := recs[0]
	if r.PlannedBy != caller || r.ApprovedAt == nil || r.BackupID == "" || r.Digest == "" || r.TaskID != "CW-1" {
		t.Fatalf("record = %+v", r)
	}
	if recs, _ := e.History(1); len(recs) != 1 || recs[0].Name != "second" {
		t.Fatalf("limit = %v", recs)
	}
}
