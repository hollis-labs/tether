package config

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// skipAsRoot skips a test that needs a directory this user cannot write to:
// root can write anywhere.
func skipAsRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("write permission cannot be taken away from root")
	}
}

// liveShape builds a catalog shaped like the live one that exposed the bug
// (CW-20261003-0092): 23 registered projects, 7 of them with a repo_root that
// does not exist (some whose parent directory exists, some whose parent does
// not), and 16 with a real directory. Everything is under writable temp
// directories, so an agent could create every missing root.
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
	// Dead: the first four have an existing parent (repos/), the rest do not
	// (the chain missing-parent/nested is missing too).
	for i := 0; i < 7; i++ {
		id := "dead-" + string(rune('a'+i))
		root := filepath.Join(base, "repos", id)
		if i >= 4 {
			root = filepath.Join(base, "missing-parent", "nested", id)
		}
		s.dead[id] = root
		s.cat.Projects[id] = Project{RepoRoot: root}
	}
	return s
}

func canonical(t *testing.T, p string) string {
	t.Helper()
	c, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func sameSet(a, b []string) bool {
	x, y := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	return strings.Join(x, "\n") == strings.Join(y, "\n")
}

// A missing root is not safe to leave alone: the sandbox is the whole host,
// writable, so an agent can create the root and a .tether in it, and the next
// catalog load reads that layer for the project (a planted agent with bypass
// permissions, reproduced). Where an agent could create the root, protection
// creates it with only .tether in it and anchors that layer like any other. The
// live-shape catalog, all 7 dead roots under writable parents, no longer aborts.
func TestPrepareCatalogProtectionAnchorsEveryMissingRootAnAgentCouldCreate(t *testing.T) {
	s := newLiveShape(t)
	got, err := PrepareCatalogProtection(s.catalogRoot, s.cat, "")
	if err != nil {
		t.Fatalf("a dead project aborted protection: %v", err)
	}
	if len(got.Skipped) != 0 {
		t.Fatalf("skipped = %+v; every root here is one an agent could create, none may be left open", got.Skipped)
	}

	// Every layer, existing or created, is anchored: the catalog root, the user
	// layer, and one .tether per project, dead or alive.
	want := []string{canonical(t, s.catalogRoot), canonical(t, filepath.Join(s.home, ".tether"))}
	for _, root := range s.existing {
		want = append(want, canonical(t, filepath.Join(root, ".tether")))
	}
	for id, root := range s.dead {
		layer := filepath.Join(root, ".tether")
		info, err := os.Stat(layer)
		if err != nil || !info.IsDir() {
			t.Fatalf("project %s: the missing root was not given an anchored layer: %v", id, err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("project %s: layer mode = %v; want 0700", id, info.Mode().Perm())
		}
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) != 1 || entries[0].Name() != ".tether" {
			t.Fatalf("project %s: the created root must hold only .tether, has %v (%v)", id, entries, err)
		}
		want = append(want, canonical(t, layer))
	}
	if !sameSet(got.Dirs, want) {
		t.Fatalf("anchored dirs = %q\nwant          %q", got.Dirs, want)
	}

	// The creation is reported, per project, and in Created.
	if len(got.CreatedRoots) != 7 {
		t.Fatalf("created roots = %+v; want the 7 dead projects", got.CreatedRoots)
	}
	reported := map[string]CreatedProjectRoot{}
	for _, c := range got.CreatedRoots {
		reported[c.Project] = c
	}
	for id, root := range s.dead {
		c, ok := reported[id]
		if !ok || c.RepoRoot != root || !strings.Contains(c.Reason, "does not exist") || !strings.Contains(c.Reason, "only an empty .tether") {
			t.Fatalf("project %s: report = %+v", id, c)
		}
	}
	created := map[string]bool{}
	for _, c := range got.Created {
		created[c] = true
	}
	for _, root := range s.dead {
		if !created[filepath.Join(root, ".tether")] {
			t.Fatalf("a created layer is not in Created: %q", got.Created)
		}
	}

	// A second call: the roots exist now, so nothing is created and the layers are
	// still anchored.
	again, err := PrepareCatalogProtection(s.catalogRoot, s.cat, "")
	if err != nil || len(again.Created) != 0 || len(again.CreatedRoots) != 0 || !sameSet(again.Dirs, want) {
		t.Fatalf("second call: created=%q roots=%d err=%v dirs match=%v", again.Created, len(again.CreatedRoots), err, sameSet(again.Dirs, want))
	}
}

// Only a root that no agent could create is left alone: the nearest existing
// directory above it is not writable by this user, so there is nothing to plant
// into. It is reported, and nothing is created under it.
func TestPrepareCatalogProtectionSkipsOnlyARootNoAgentCouldCreate(t *testing.T) {
	skipAsRoot(t)
	s := newLiveShape(t)
	locked := filepath.Join(t.TempDir(), "locked")
	if err := os.MkdirAll(filepath.Join(locked, "inside"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(locked, "inside"), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(locked, "inside"), 0o750) })
	if err := os.Chmod(locked, 0o500); err != nil { // can enter and read, cannot create
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o750) })
	s.cat.Projects["ro-direct"] = Project{RepoRoot: filepath.Join(locked, "ro-direct")}
	s.cat.Projects["ro-deep"] = Project{RepoRoot: filepath.Join(locked, "inside", "a", "b")}

	got, err := PrepareCatalogProtection(s.catalogRoot, s.cat, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Skipped) != 2 {
		t.Fatalf("skipped = %+v; want exactly the two roots under the read-only directories", got.Skipped)
	}
	byID := map[string]SkippedProjectLayer{}
	for _, sk := range got.Skipped {
		byID[sk.Project] = sk
	}
	for id, ancestor := range map[string]string{"ro-direct": locked, "ro-deep": filepath.Join(locked, "inside")} {
		sk, ok := byID[id]
		if !ok || !strings.Contains(sk.Reason, "does not exist") || !strings.Contains(sk.Reason, "an agent cannot create it either") || !strings.Contains(sk.Reason, ancestor+" is not writable") {
			t.Fatalf("project %s: skip = %+v; want a reason naming %s", id, sk, ancestor)
		}
	}
	for _, root := range []string{filepath.Join(locked, "ro-direct"), filepath.Join(locked, "inside", "a")} {
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Fatalf("%s was created although no agent could create it (%v)", root, err)
		}
	}
	if len(got.CreatedRoots) != 7 { // the 7 writable dead projects of the live shape are still anchored
		t.Fatalf("created roots = %d; want the 7 writable dead ones", len(got.CreatedRoots))
	}
}

// The project a launch is for is different: if ITS root is unusable the launch
// fails with an error naming the project and the path, its root is never created,
// and, because every root is decided before anything is created, the failed call
// leaves nothing behind anywhere: no .tether in the live projects, no chain for
// the other dead projects, no user layer.
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
	for id, root := range s.dead {
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Fatalf("project %s: the failed call created the dead root %s (%v)", id, root, err)
		}
	}
	if _, err := os.Stat(filepath.Join(s.home, ".tether")); !os.IsNotExist(err) {
		t.Fatalf("the failed call created the user layer (%v)", err)
	}

	// A launch for an existing project is unaffected by the dead ones.
	got, err := PrepareCatalogProtection(s.catalogRoot, s.cat, "live-c")
	if err != nil || len(got.CreatedRoots) != 7 {
		t.Fatalf("launching live-c: created roots=%d err=%v; want success with the 7 dead roots anchored", len(got.CreatedRoots), err)
	}
}

// A file in the way: a root that is a regular file, or runs through one. Under a
// directory an agent can write to, it could replace the file with a directory and
// plant a layer, and the user's file is not Tether's to delete, so protection
// fails closed naming the project (and creates nothing). For the launching
// project it is the ordinary typed error.
func TestPrepareCatalogProtectionAFileInTheWay(t *testing.T) {
	base := t.TempDir()
	t.Setenv("HOME", filepath.Join(base, "home"))
	catalogRoot := filepath.Join(base, "catalog")
	for _, d := range []string{filepath.Join(base, "home"), catalogRoot, filepath.Join(base, "repos", "fine")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(base, "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cat := &Catalog{Projects: map[string]Project{
		"fine":    {RepoRoot: filepath.Join(base, "repos", "fine")},
		"isfile":  {RepoRoot: file},
		"underit": {RepoRoot: filepath.Join(file, "child")},
	}}

	_, err := PrepareCatalogProtection(catalogRoot, cat, "")
	var layerErr *UnprotectableLayerError
	if !errors.As(err, &layerErr) || layerErr.Project != "isfile" || !strings.Contains(err.Error(), "a file is in the way") || !strings.Contains(err.Error(), `"isfile"`) {
		t.Fatalf("err = %v; want an *UnprotectableLayerError naming isfile", err)
	}
	if _, err := os.Stat(filepath.Join(base, "repos", "fine", ".tether")); !os.IsNotExist(err) {
		t.Fatalf("the refused call created a layer (%v)", err)
	}

	var rootErr *ProjectRootError
	if _, err := PrepareCatalogProtection(catalogRoot, cat, "isfile"); !errors.As(err, &rootErr) || rootErr.Reason != "is not a directory" {
		t.Fatalf("launching the file project: %v", err)
	}
	delete(cat.Projects, "isfile")
	if _, err := PrepareCatalogProtection(catalogRoot, cat, "launching-other"); !errors.As(err, &layerErr) || layerErr.Project != "underit" {
		t.Fatalf("a root under a file: err = %v; want an *UnprotectableLayerError naming underit", err)
	}
}

// Where the file's own directory is not writable, an agent cannot replace the
// file, so there is nothing to plant into and the project is skipped.
func TestPrepareCatalogProtectionAFileInAnUnwritableDirectoryIsSkipped(t *testing.T) {
	skipAsRoot(t)
	base := t.TempDir()
	t.Setenv("HOME", filepath.Join(base, "home"))
	catalogRoot := filepath.Join(base, "catalog")
	locked := filepath.Join(base, "locked")
	for _, d := range []string{filepath.Join(base, "home"), catalogRoot, locked} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(locked, "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o750) })
	cat := &Catalog{Projects: map[string]Project{"isfile": {RepoRoot: file}, "underit": {RepoRoot: filepath.Join(file, "child")}}}
	got, err := PrepareCatalogProtection(catalogRoot, cat, "")
	if err != nil || len(got.Skipped) != 2 {
		t.Fatalf("skipped=%+v err=%v; want both skipped: an agent cannot replace a file in a directory it cannot write", got.Skipped, err)
	}
	reasons := map[string]string{}
	for _, sk := range got.Skipped {
		reasons[sk.Project] = sk.Reason
	}
	if !strings.HasPrefix(reasons["isfile"], "is not a directory") || !strings.HasPrefix(reasons["underit"], "does not exist") {
		t.Fatalf("reasons = %v", reasons)
	}
}

// A dangling symlink as a root: an agent's mkdir through it lands where it
// points, so that is where the layer is anchored, and the link is left alone.
func TestPrepareCatalogProtectionAnchorsTheTargetOfADanglingSymlinkRoot(t *testing.T) {
	base := t.TempDir()
	t.Setenv("HOME", filepath.Join(base, "home"))
	catalogRoot := filepath.Join(base, "catalog")
	for _, d := range []string{filepath.Join(base, "home"), catalogRoot} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(base, "nowhere", "target")
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	cat := &Catalog{Projects: map[string]Project{"dangling": {RepoRoot: link}}}
	got, err := PrepareCatalogProtection(catalogRoot, cat, "")
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the symlink root was replaced or removed (%v)", err)
	}
	layer := filepath.Join(target, ".tether")
	if fi, err := os.Stat(layer); err != nil || !fi.IsDir() {
		t.Fatalf("the layer was not created where the link lands: %v", err)
	}
	// The path the catalog loader reads (through the link) is the anchored directory.
	viaLink, err1 := os.Stat(filepath.Join(link, ".tether"))
	direct, err2 := os.Stat(layer)
	if err1 != nil || err2 != nil || !os.SameFile(viaLink, direct) {
		t.Fatalf("the loader's path through the link is not the anchored layer (%v %v)", err1, err2)
	}
	found := false
	for _, d := range got.Dirs {
		if d == canonical(t, layer) {
			found = true
		}
	}
	if !found || len(got.CreatedRoots) != 1 {
		t.Fatalf("dirs = %q created roots = %+v; want the layer anchored and reported", got.Dirs, got.CreatedRoots)
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
// dead project, and anchors the created layers too.
func TestCatalogProtectionDirsAnchorsMissingRootsToo(t *testing.T) {
	s := newLiveShape(t)
	dirs, err := CatalogProtectionDirs(s.catalogRoot, s.cat)
	if want := 2 + len(s.existing) + len(s.dead); err != nil || len(dirs) != want {
		t.Fatalf("dirs = %d, err = %v; want %d and no error", len(dirs), err, want)
	}
}

// What protection cannot examine is not "missing": a root it cannot stat for
// another reason (permissions), and a project rooted at the filesystem root,
// still fail closed, and name the project. (A relative repo_root is not such a
// case: Expand resolves it against the working directory, so it is an ordinary
// absolute path.)
func TestPrepareCatalogProtectionFailsClosedOnWhatItCannotSee(t *testing.T) {
	s := newLiveShape(t)
	s.cat.Projects["rootfs"] = Project{RepoRoot: "/"}
	if _, err := PrepareCatalogProtection(s.catalogRoot, s.cat, ""); err == nil || !strings.Contains(err.Error(), `project "rootfs"`) {
		t.Fatalf("a project rooted at /: err = %v; want a refusal naming the project", err)
	}
	delete(s.cat.Projects, "rootfs")

	skipAsRoot(t)
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

// agentCanWrite is the kernel's answer for this user, not a mode guess.
func TestAgentCanWrite(t *testing.T) {
	dir := t.TempDir()
	if !agentCanWrite(dir) {
		t.Fatal("a directory this user owns and can write is reported as not writable")
	}
	skipAsRoot(t)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o750) })
	if agentCanWrite(dir) {
		t.Fatal("a read-only directory is reported as writable")
	}
}
