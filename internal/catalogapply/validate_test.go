package catalogapply

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func exampleEngine(t *testing.T) *Engine {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "catalog")
	src, _ := filepath.Abs(filepath.Join("..", "..", "examples", "catalog"))
	if out, err := exec.Command("cp", "-r", src, root).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return &Engine{Root: root, BackupRoot: filepath.Join(dir, "backups")}
}

func TestValidateLiveAndBundle(t *testing.T) {
	e := exampleEngine(t)
	live, err := e.ValidateBundle(nil, Options{})
	if err != nil || !live.OK {
		t.Fatalf("live: %+v err=%v", live, err)
	}

	good := []File{{Path: "agents/helper.yaml", Content: "id: helper\nname: Helper\n"}}
	rep, err := e.ValidateBundle(good, Options{})
	if err != nil || !rep.OK {
		t.Fatalf("good draft: %+v err=%v", rep, err)
	}

	// A launch that names an agent the draft does not define fails, and the
	// live catalog is untouched either way.
	bad := []File{{Path: "launches/draft.yaml", Content: "id: draft\nproject: demo\nagent: not-defined\nprovider: claude-code\n"}}
	rep, err = e.ValidateBundle(bad, Options{})
	if err != nil || rep.OK || rep.Failures == 0 {
		t.Fatalf("bad draft should fail validation: %+v err=%v", rep, err)
	}
	for _, rel := range []string{"launches/draft.yaml", "agents/helper.yaml"} {
		if _, err := os.Stat(filepath.Join(e.Root, rel)); err == nil {
			t.Fatalf("validation wrote %s into the live catalog", rel)
		}
	}
	if len(e.plans) != 0 {
		t.Fatal("validation stored a plan")
	}
}

func TestValidateBundleAppliesTheInputRules(t *testing.T) {
	e := exampleEngine(t)
	for name, files := range map[string][]File{
		"outside allowlist": {{Path: "credentials/x.yaml", Content: "a: b\n"}},
		"secret":            {{Path: "agents/s.yaml", Content: "api_key: sk-abcdefghijklmnopqrstuvwxyz0123\n"}},
		"traversal":         {{Path: "../x.yaml", Content: "a: b\n"}},
	} {
		if _, err := e.ValidateBundle(files, Options{}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := e.ValidateBundle([]File{{Path: "agents/none.yaml", Delete: true}}, Options{}); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("delete of a missing file: %v", err)
	}
}
