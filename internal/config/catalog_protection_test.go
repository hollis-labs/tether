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
		if layerEntries, err := os.ReadDir(layer); err != nil || len(layerEntries) != 1 || layerEntries[0].Name() != PlaceholderMarker {
			t.Fatalf("project %s: the created layer must hold only %s, has %v (%v)", id, PlaceholderMarker, layerEntries, err)
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
		if !ok || c.RepoRoot != root || !strings.Contains(c.Reason, "does not exist") || !strings.Contains(c.Reason, "only a read-only .tether") {
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
	// still anchored. They are still placeholders, and still reported as such: the
	// report does not depend on remembering the call that created them.
	again, err := PrepareCatalogProtection(s.catalogRoot, s.cat, "")
	if err != nil || len(again.Created) != 0 || len(again.CreatedRoots) != 7 || !sameSet(again.Dirs, want) {
		t.Fatalf("second call: created=%q roots=%d err=%v dirs match=%v", again.Created, len(again.CreatedRoots), err, sameSet(again.Dirs, want))
	}
	for _, c := range again.CreatedRoots {
		if !strings.Contains(c.Reason, "still the placeholder") {
			t.Fatalf("second call: %+v does not say the root is still a placeholder", c)
		}
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

// The refusal for the launching project has to survive the protection that comes
// before it. Any protected call creates every other project's missing root, and
// the first one after the daemon starts does it for all of them, so a launch for a
// dead project would otherwise find its root there, empty, and run in it: the
// typed error would never fire. The placeholder is told by its marker, so it holds
// across a restart; a directory that merely holds an empty .tether (what
// protection leaves in every project root) is an ordinary project.
func TestPrepareCatalogProtectionTheLaunchingProjectWithAPlaceholderRootIsStillRefused(t *testing.T) {
	s := newLiveShape(t)
	if _, err := PrepareCatalogProtection(s.catalogRoot, s.cat, "live-a"); err != nil {
		t.Fatal(err)
	}
	root := s.dead["dead-b"]
	var rootErr *ProjectRootError
	_, err := PrepareCatalogProtection(s.catalogRoot, s.cat, "dead-b")
	if !errors.As(err, &rootErr) || rootErr.Project != "dead-b" || rootErr.Root != root || !strings.Contains(rootErr.Reason, "placeholder") || !strings.Contains(rootErr.Reason, PlaceholderMarker) {
		t.Fatalf("err = %v; want a *ProjectRootError naming dead-b and the placeholder", err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 1 || entries[0].Name() != ".tether" {
		t.Fatalf("the refused call changed the placeholder: %v (%v)", entries, err)
	}

	// An ordinary project whose root holds only the empty .tether protection leaves
	// in every root is launchable, and is not reported as a placeholder.
	got, err := PrepareCatalogProtection(s.catalogRoot, s.cat, "live-c")
	if err != nil || len(got.CreatedRoots) != 7 {
		t.Fatalf("launching live-c: created roots=%d err=%v; want success and only the 7 dead projects reported", len(got.CreatedRoots), err)
	}
	for _, c := range got.CreatedRoots {
		if _, live := s.existing[c.Project]; live {
			t.Fatalf("an ordinary project is reported as a placeholder: %+v", c)
		}
	}

	// Once the repository is restored into the root, it is no longer a placeholder.
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("restored\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	got, err = PrepareCatalogProtection(s.catalogRoot, s.cat, "dead-b")
	if err != nil {
		t.Fatalf("launching dead-b with its repository back: %v", err)
	}
	for _, c := range got.CreatedRoots {
		if c.Project == "dead-b" {
			t.Fatalf("a restored project is still reported as a placeholder: %+v", c)
		}
	}

	// So is a root whose layer has been given content: that is somebody's layer.
	other := s.dead["dead-c"]
	if err := os.MkdirAll(filepath.Join(other, ".tether", "agents"), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareCatalogProtection(s.catalogRoot, s.cat, "dead-c"); err != nil {
		t.Fatalf("launching dead-c with its own layer content: %v", err)
	}
}

// The marker must not be read as catalog content: LoadLayered reads
// <layer>/<kind>/*.yaml, and an anchored placeholder yields nothing and no error.
func TestLoadLayeredIgnoresAPlaceholderLayer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	catalogRoot := t.TempDir()
	dead := filepath.Join(t.TempDir(), "gone")
	for path, body := range map[string]string{
		filepath.Join(catalogRoot, "global.yaml"):                 "version: 0.1.0\n",
		filepath.Join(catalogRoot, "projects", "gone.yaml"):       "id: gone\nname: Gone\nrepo_root: " + dead + "\n",
		filepath.Join(catalogRoot, "providers", "cli.yaml"):       "id: cli\ntype: cli\ncommand: echo\n",
		filepath.Join(catalogRoot, "agents", "system-agent.yaml"): "id: system-agent\nname: System\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cat, err := Load(catalogRoot)
	if err != nil {
		t.Fatal(err)
	}
	got, err := PrepareCatalogProtection(catalogRoot, cat, "")
	if err != nil || len(got.CreatedRoots) != 1 || !placeholderRoot(dead) {
		t.Fatalf("created roots = %+v, err = %v, placeholder = %v; want the dead root made into a placeholder", got.CreatedRoots, err, placeholderRoot(dead))
	}
	layered, err := LoadLayered(catalogRoot)
	if err != nil {
		t.Fatalf("LoadLayered with a placeholder layer: %v", err)
	}
	if _, ok := layered.Agents["system-agent"]; !ok || len(layered.Agents) != 1 {
		t.Fatalf("agents = %v; want only the system agent", layered.Agents)
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

// systemDirNoAgentCanWrite finds a directory that a process running as this user
// can neither write, nor chmod, nor move aside: it and every directory above it is
// owned by someone else and not writable. That takes a real system directory,
// because every directory a test can make is owned by this user, and so is one an
// agent can get past. The test is skipped where there is none.
func systemDirNoAgentCanWrite(t *testing.T) string {
	t.Helper()
	skipAsRoot(t)
	for _, dir := range []string{"/usr/share", "/usr/lib", "/usr/include", "/opt", "/etc"} {
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() || agentCanWrite(dir) {
			continue
		}
		if past, err := agentGetsPast(dir); err == nil && past == "" {
			return dir
		}
	}
	t.Skip("no system directory that this user can neither write nor get past")
	return ""
}

// Only a root that no agent could create is left alone. The agent is this user, so
// "not writable" is not enough: the nearest existing directory above the root and
// everything up to / must be out of this user's hands (not owned, not writable).
// It is reported, and nothing is created under it.
func TestPrepareCatalogProtectionSkipsOnlyARootNoAgentCouldCreate(t *testing.T) {
	sys := systemDirNoAgentCanWrite(t)
	s := newLiveShape(t)
	direct := filepath.Join(sys, "tether-protection-test-no-such-dir")
	deep := filepath.Join(sys, "tether-protection-test-no-such-dir", "a", "b")
	s.cat.Projects["ro-direct"] = Project{RepoRoot: direct}
	s.cat.Projects["ro-deep"] = Project{RepoRoot: deep}

	got, err := PrepareCatalogProtection(s.catalogRoot, s.cat, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Skipped) != 2 {
		t.Fatalf("skipped = %+v; want exactly the two roots under %s", got.Skipped, sys)
	}
	for _, sk := range got.Skipped {
		if !strings.Contains(sk.Reason, "does not exist") || !strings.Contains(sk.Reason, "an agent cannot create it either") || !strings.Contains(sk.Reason, sys+" is not writable") {
			t.Fatalf("skip = %+v; want a reason naming %s", sk, sys)
		}
	}
	if _, err := os.Stat(direct); !os.IsNotExist(err) {
		t.Fatalf("%s was created although no agent could create it (%v)", direct, err)
	}
	if len(got.CreatedRoots) != 7 { // the 7 writable dead projects of the live shape are still anchored
		t.Fatalf("created roots = %d; want the 7 writable dead ones", len(got.CreatedRoots))
	}
}

// A file in a directory no agent can touch cannot be replaced either, so the
// project is skipped.
func TestPrepareCatalogProtectionAFileInADirectoryNoAgentCanTouchIsSkipped(t *testing.T) {
	sys := systemDirNoAgentCanWrite(t)
	var file string
	entries, err := os.ReadDir(sys)
	if err != nil {
		t.Skip(err)
	}
	for _, e := range entries {
		if e.Type().IsRegular() {
			file = filepath.Join(sys, e.Name())
			break
		}
	}
	if file == "" {
		t.Skipf("no regular file directly in %s", sys)
	}
	base := t.TempDir()
	t.Setenv("HOME", filepath.Join(base, "home"))
	catalogRoot := filepath.Join(base, "catalog")
	for _, d := range []string{filepath.Join(base, "home"), catalogRoot} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	cat := &Catalog{Projects: map[string]Project{"isfile": {RepoRoot: file}, "underit": {RepoRoot: filepath.Join(file, "child")}}}
	got, err := PrepareCatalogProtection(catalogRoot, cat, "")
	if err != nil || len(got.Skipped) != 2 {
		t.Fatalf("skipped=%+v err=%v; want both skipped: nothing an agent does replaces a file in %s", got.Skipped, err, sys)
	}
	reasons := map[string]string{}
	for _, sk := range got.Skipped {
		reasons[sk.Project] = sk.Reason
	}
	if !strings.HasPrefix(reasons["isfile"], "is not a directory") || !strings.HasPrefix(reasons["underit"], "does not exist") {
		t.Fatalf("reasons = %v", reasons)
	}
}

// The agent runs as the user who owns the directory, so a directory that is not
// writable is not out of its reach: it can chmod what the user owns, and rename
// any directory it can write the parent of. Skipping such a root was fail-open
// (reproduced: chmod u+w, mkdir, plant; then the next catalog load read the
// layer). Creating the root there is impossible without changing the user's
// directory, so protection fails closed, naming the project and the way past, and
// creates nothing.
func TestPrepareCatalogProtectionFailsClosedWhereAnAgentCanGetPastAnUnwritableDirectory(t *testing.T) {
	skipAsRoot(t)
	s := newLiveShape(t)
	locked := filepath.Join(t.TempDir(), "locked")
	inside := filepath.Join(locked, "inside")
	if err := os.MkdirAll(inside, 0o750); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(locked, "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{inside, locked} { // can enter and read, cannot create
		if err := os.Chmod(d, 0o500); err != nil {
			t.Fatal(err)
		}
		d := d
		t.Cleanup(func() { _ = os.Chmod(d, 0o750) })
	}
	for name, root := range map[string]string{
		"direct":    filepath.Join(locked, "repo"),
		"deep":      filepath.Join(inside, "a", "b"),
		"isfile":    file,
		"underfile": filepath.Join(file, "child"),
	} {
		cat := &Catalog{Projects: map[string]Project{}}
		for id, r := range s.existing {
			cat.Projects[id] = Project{RepoRoot: r}
		}
		cat.Projects["ro-"+name] = Project{RepoRoot: root}
		_, err := PrepareCatalogProtection(s.catalogRoot, cat, "")
		var layerErr *UnprotectableLayerError
		if !errors.As(err, &layerErr) || layerErr.Project != "ro-"+name {
			t.Fatalf("%s: err = %v; want an *UnprotectableLayerError naming ro-%s", name, err, name)
		}
		for _, want := range []string{`"ro-` + name + `"`, "is not writable", "owned by this user", "can get past"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("%s: message %q does not mention %q", name, err.Error(), want)
			}
		}
		// the refused call created nothing, anywhere
		for id, r := range s.existing {
			if _, err := os.Stat(filepath.Join(r, ".tether")); !os.IsNotExist(err) {
				t.Fatalf("%s: the refused call created %s/.tether for %s (%v)", name, r, id, err)
			}
		}
		if _, err := os.Stat(filepath.Join(s.home, ".tether")); !os.IsNotExist(err) {
			t.Fatalf("%s: the refused call created the user layer (%v)", name, err)
		}
	}
}

// bypassVia, case by case. chain runs from the nearest existing ancestor up to /.
func TestBypassVia(t *testing.T) {
	const me, other = 1000, 0
	d := func(path string, uid uint32, sticky, writable bool) dirFacts {
		return dirFacts{path: path, uid: uid, sticky: sticky, writable: writable}
	}
	for _, c := range []struct {
		name  string
		chain []dirFacts
		want  string // "" = no way past; else a substring of the way
	}{
		{"nothing owned or writable up to /", []dirFacts{d("/usr/share", other, false, false), d("/usr", other, false, false), d("/", other, false, false)}, ""},
		{"the ancestor is owned by this user: chmod", []dirFacts{d("/h/ro", me, false, false), d("/h", other, false, false), d("/", other, false, false)}, "/h/ro is owned by this user"},
		{"owned by this user, mode 555, deep in the chain", []dirFacts{d("/a/ro", other, false, false), d("/a", me, false, false), d("/", other, false, false)}, "/a, above it, is owned by this user"},
		{"not owned, but the directory above is writable: rename it aside", []dirFacts{d("/h/ro", other, false, false), d("/h", other, false, true), d("/", other, false, false)}, "/h, above it, is writable by this user, who can rename /h/ro"},
		{"not owned, the directory above is writable but sticky (a /tmp)", []dirFacts{d("/tmp/ro", other, false, false), d("/tmp", other, true, true), d("/", other, false, false)}, ""},
		{"sticky above, but the grandparent is writable and not sticky", []dirFacts{d("/x/tmp/ro", other, false, false), d("/x/tmp", other, true, true), d("/x", other, false, true), d("/", other, false, false)}, "/x, above it, is writable by this user, who can rename /x/tmp"},
		{"sticky above, but this user owns it", []dirFacts{d("/tmp/ro", other, false, false), d("/tmp", me, true, true), d("/", other, false, false)}, "/tmp, above it, is owned by this user"},
		{"the first directory is itself writable", []dirFacts{d("/w", other, false, true), d("/", other, false, false)}, "/w is writable by this user"},
		{"root owns /, and this user is root", []dirFacts{d("/usr/share", 0, false, false), d("/", 0, false, false)}, ""},
	} {
		euid := uint32(me)
		if c.name == "root owns /, and this user is root" {
			euid = 0
			c.want = "/usr/share is owned by this user" // root owns everything root-owned
		}
		got := bypassVia(c.chain, euid)
		if (c.want == "") != (got == "") || (c.want != "" && !strings.Contains(got, c.want)) {
			t.Errorf("%s: bypassVia = %q; want %q", c.name, got, c.want)
		}
	}
}

// agentGetsPast examines the real directories: an owned read-only directory is a
// way past, a directory nothing in reach controls is not, and what cannot be
// examined is an error.
func TestAgentGetsPast(t *testing.T) {
	skipAsRoot(t)
	locked := filepath.Join(t.TempDir(), "locked")
	if err := os.MkdirAll(locked, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o750) })
	if got, err := agentGetsPast(locked); err != nil || !strings.Contains(got, "is owned by this user") {
		t.Fatalf("an owned read-only directory: got %q, err %v; want a way past through ownership", got, err)
	}
	if got, err := agentGetsPast(filepath.Join(locked, "does-not-exist")); err == nil {
		t.Fatalf("a directory that does not exist: got %q; want an error: nothing was examined", got)
	}
	if sys := systemDirNoAgentCanWrite(t); sys != "" {
		if got, err := agentGetsPast(sys); err != nil || got != "" {
			t.Fatalf("%s: got %q, err %v; want no way past", sys, got, err)
		}
	}
}
