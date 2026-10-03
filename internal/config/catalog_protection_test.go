package config

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// liveShape builds a catalog shaped like the live one that exposed the bug
// (CW-20261003-0092): 23 registered projects, 7 of them with a repo_root that
// does not exist (some whose parent directory exists, some whose parent does
// not), and 16 with a real directory.
type liveShape struct {
	cat         *Catalog
	catalogRoot string
	home        string
	existing    map[string]string // project id -> repo_root
	dead        map[string]string // project id -> repo_root that does not exist
}

func newLiveShape(t *testing.T) *liveShape {
	t.Helper()
	base := t.TempDir()
	s := &liveShape{
		cat:         &Catalog{Projects: map[string]Project{}},
		catalogRoot: filepath.Join(base, "catalog"),
		home:        filepath.Join(base, "home"),
		existing:    map[string]string{},
		dead:        map[string]string{},
	}
	for _, d := range []string{s.catalogRoot, s.home} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", s.home)
	for i := 0; i < 16; i++ {
		id := "live-" + string(rune('a'+i))
		root := filepath.Join(base, "repos", id)
		if err := os.MkdirAll(root, 0o750); err != nil {
			t.Fatal(err)
		}
		s.existing[id] = root
		s.cat.Projects[id] = Project{RepoRoot: root}
	}
	// Dead: the first half have an existing parent (repos/), the rest do not.
	for i := 0; i < 7; i++ {
		id := "dead-" + string(rune('a'+i))
		root := filepath.Join(base, "repos", id)
		if i >= 4 {
			root = filepath.Join(base, "missing-parent", id)
		}
		s.dead[id] = root
		s.cat.Projects[id] = Project{RepoRoot: root}
	}
	return s
}

func sortedKeys(m map[string]string) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func canonical(t *testing.T, p string) string {
	t.Helper()
	c, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A catalog project whose repo_root is gone must not abort protection for every
// other project: it is skipped and reported, each existing layer is protected,
// and nothing is created under a root that does not exist.
func TestPrepareCatalogProtectionSkipsDeadProjectsAndProtectsExactlyTheExistingLayers(t *testing.T) {
	s := newLiveShape(t)
	got, err := PrepareCatalogProtection(s.catalogRoot, s.cat, "")
	if err != nil {
		t.Fatalf("a dead project aborted protection: %v", err)
	}

	// The skip is reported: every dead project, with its path and why, in id order.
	if len(got.Skipped) != 7 {
		t.Fatalf("skipped = %+v; want the 7 dead projects", got.Skipped)
	}
	for i, id := range sortedKeys(s.dead) {
		sk := got.Skipped[i]
		if sk.Project != id || sk.RepoRoot != s.dead[id] || sk.Reason != "does not exist" {
			t.Fatalf("skipped[%d] = %+v; want %s %s does not exist", i, sk, id, s.dead[id])
		}
	}

	// Exactly the existing layers are protected: the catalog root, the user
	// layer and one .tether per existing project (the catalog's sub-roots sit
	// inside the catalog root, which already covers them).
	want := []string{canonical(t, s.catalogRoot), canonical(t, filepath.Join(s.home, ".tether"))}
	for _, id := range sortedKeys(s.existing) {
		want = append(want, canonical(t, filepath.Join(s.existing[id], ".tether")))
	}
	gotDirs := append([]string(nil), got.Dirs...)
	sort.Strings(gotDirs)
	sort.Strings(want)
	if strings.Join(gotDirs, "\n") != strings.Join(want, "\n") {
		t.Fatalf("protected dirs = %q\nwant         %q", gotDirs, want)
	}

	// Nothing was created under a root that does not exist, nor above it.
	for id, root := range s.dead {
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Fatalf("project %s: a missing repo_root %s was created (%v)", id, root, err)
		}
		if strings.Contains(root, "missing-parent") {
			if _, err := os.Stat(filepath.Dir(root)); !os.IsNotExist(err) {
				t.Fatalf("project %s: its missing parent %s was created (%v)", id, filepath.Dir(root), err)
			}
		}
	}

	// The empty layers that had to be created are reported, for existing roots only.
	created := map[string]bool{}
	for _, c := range got.Created {
		created[c] = true
	}
	for id, root := range s.existing {
		if !created[filepath.Join(root, ".tether")] {
			t.Fatalf("project %s: its layer was not created or not reported: created=%q", id, got.Created)
		}
	}

	// A second call changes nothing: the layers exist, the skips are the same.
	again, err := PrepareCatalogProtection(s.catalogRoot, s.cat, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Created) != 0 || len(again.Skipped) != 7 || strings.Join(again.Dirs, "\n") != strings.Join(got.Dirs, "\n") {
		t.Fatalf("second call: created=%q skipped=%d dirs differ=%v", again.Created, len(again.Skipped), strings.Join(again.Dirs, "\n") != strings.Join(got.Dirs, "\n"))
	}
}

// The project a launch is for is different: if ITS root is unusable the launch
// fails with an error naming the project and the path, and, because every root
// is checked before anything is created, the failed call leaves no directory
// behind in the other projects.
func TestPrepareCatalogProtectionTheLaunchingProjectWithADeadRootIsATypedErrorWithNoSideEffects(t *testing.T) {
	s := newLiveShape(t)
	_, err := PrepareCatalogProtection(s.catalogRoot, s.cat, "dead-b")
	var rootErr *ProjectRootError
	if !errors.As(err, &rootErr) {
		t.Fatalf("err = %v; want a *ProjectRootError", err)
	}
	if rootErr.Project != "dead-b" || rootErr.Root != s.dead["dead-b"] || rootErr.Reason != "does not exist" {
		t.Fatalf("error = %+v; want dead-b %s does not exist", rootErr, s.dead["dead-b"])
	}
	for _, want := range []string{`"dead-b"`, s.dead["dead-b"], "does not exist"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("message %q does not mention %s", err.Error(), want)
		}
	}
	for id, root := range s.existing {
		if _, err := os.Stat(filepath.Join(root, ".tether")); !os.IsNotExist(err) {
			t.Fatalf("project %s: the failed call created %s/.tether (%v)", id, root, err)
		}
	}
	if _, err := os.Stat(filepath.Join(s.home, ".tether")); !os.IsNotExist(err) {
		t.Fatalf("the failed call created the user layer (%v)", err)
	}

	// A launch for an existing project is unaffected by the dead ones.
	got, err := PrepareCatalogProtection(s.catalogRoot, s.cat, "live-c")
	if err != nil || len(got.Skipped) != 7 {
		t.Fatalf("launching live-c: skipped=%d err=%v; want success with the 7 dead skipped", len(got.Skipped), err)
	}
}

// A repo_root that is a regular file, or sits under one, is as unusable as a
// missing one: skipped for others, a typed error for the launching project.
func TestPrepareCatalogProtectionARootThatIsAFileOrUnderOne(t *testing.T) {
	base := t.TempDir()
	t.Setenv("HOME", filepath.Join(base, "home"))
	if err := os.MkdirAll(filepath.Join(base, "home"), 0o750); err != nil {
		t.Fatal(err)
	}
	catalogRoot := filepath.Join(base, "catalog")
	if err := os.MkdirAll(catalogRoot, 0o750); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(base, "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cat := &Catalog{Projects: map[string]Project{
		"isfile":  {RepoRoot: file},
		"underit": {RepoRoot: filepath.Join(file, "child")},
	}}
	got, err := PrepareCatalogProtection(catalogRoot, cat, "")
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[string]string{}
	for _, sk := range got.Skipped {
		reasons[sk.Project] = sk.Reason
	}
	if reasons["isfile"] != "is not a directory" || reasons["underit"] != "does not exist" {
		t.Fatalf("reasons = %v", reasons)
	}
	var rootErr *ProjectRootError
	if _, err := PrepareCatalogProtection(catalogRoot, cat, "isfile"); !errors.As(err, &rootErr) || rootErr.Reason != "is not a directory" {
		t.Fatalf("launching the file project: %v", err)
	}
}

// Protection of a layer that exists is not weakened by the change: an existing
// .tether keeps its contents, is listed as protected, and is not recreated.
func TestPrepareCatalogProtectionKeepsAnExistingLayerAsItIs(t *testing.T) {
	s := newLiveShape(t)
	layer := filepath.Join(s.existing["live-a"], ".tether")
	if err := os.MkdirAll(layer, 0o750); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(layer, "config.yaml")
	if err := os.WriteFile(marker, []byte("keep: me\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	got, err := PrepareCatalogProtection(s.catalogRoot, s.cat, "live-a")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range got.Dirs {
		if d == canonical(t, layer) {
			found = true
		}
	}
	if !found {
		t.Fatalf("an existing layer is not protected: %q", got.Dirs)
	}
	for _, c := range got.Created {
		if c == layer {
			t.Fatalf("an existing layer was reported as created: %q", got.Created)
		}
	}
	if b, err := os.ReadFile(marker); err != nil || string(b) != "keep: me\n" {
		t.Fatalf("an existing layer's content changed: %q %v", b, err)
	}
}

// CatalogProtectionDirs, the signature other callers use, no longer fails on a
// dead project either.
func TestCatalogProtectionDirsToleratesDeadProjects(t *testing.T) {
	s := newLiveShape(t)
	dirs, err := CatalogProtectionDirs(s.catalogRoot, s.cat)
	if err != nil || len(dirs) != 2+len(s.existing) {
		t.Fatalf("dirs = %d, err = %v; want %d and no error", len(dirs), err, 2+len(s.existing))
	}
}

// What protection cannot examine is not "missing": a root it cannot stat for
// another reason (permissions), and a project rooted at the filesystem root,
// still fail closed, and name the project. (A relative repo_root is not such a
// case: Expand resolves it against the working directory, so it is an ordinary
// absolute path that either exists or is skipped.)
func TestPrepareCatalogProtectionFailsClosedOnWhatItCannotSee(t *testing.T) {
	s := newLiveShape(t)
	s.cat.Projects["rootfs"] = Project{RepoRoot: "/"}
	if _, err := PrepareCatalogProtection(s.catalogRoot, s.cat, ""); err == nil || !strings.Contains(err.Error(), `project "rootfs"`) {
		t.Fatalf("a project rooted at /: err = %v; want a refusal naming the project", err)
	}
	delete(s.cat.Projects, "rootfs")

	if os.Geteuid() == 0 {
		t.Skip("permission errors cannot be provoked as root")
	}
	locked := filepath.Join(t.TempDir(), "locked")
	if err := os.MkdirAll(filepath.Join(locked, "repo"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o750) })
	s.cat.Projects["locked"] = Project{RepoRoot: filepath.Join(locked, "repo")}
	if _, err := PrepareCatalogProtection(s.catalogRoot, s.cat, ""); err == nil || !strings.Contains(err.Error(), `project "locked"`) {
		t.Fatalf("unreadable repo_root: err = %v; want a fail-closed refusal naming the project", err)
	}
}
