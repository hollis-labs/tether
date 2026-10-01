//go:build linux

package agentops

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// guardLayout is a protected "catalog" with an agent in it, and a place outside
// it to write.
func guardLayout(t *testing.T) (catalog, outside string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	catalog, outside = filepath.Join(base, "catalog"), filepath.Join(base, "outside")
	for _, d := range []string{filepath.Join(catalog, "agents"), outside} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(catalog, "agents", "keep.yaml"), []byte("id: keep\nname: Keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return catalog, outside
}

func catalogUntouched(t *testing.T, catalog string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(catalog, "agents"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "keep.yaml" {
		t.Fatalf("the catalog's agents/ changed: %v", entries)
	}
	if data, _ := os.ReadFile(filepath.Join(catalog, "agents", "keep.yaml")); string(data) != "id: keep\nname: Keep\n" {
		t.Fatalf("the catalog's agent file changed: %q", data)
	}
}

// Outside the protected directories CreateGuarded writes what Create writes,
// including the directories it has to make.
func TestCreateGuarded_WritesWhatCreateWritesOutsideProtectedDirs(t *testing.T) {
	catalog, outside := guardLayout(t)
	p := Params{Name: "N", Roles: []string{"worker"}, SystemPrompt: "sp"}
	want, err := Create(filepath.Join(outside, "plain", ".tether"), "a", p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := CreateGuarded(filepath.Join(outside, "guarded", "deep", ".tether"), "a", p, []string{catalog})
	if err != nil {
		t.Fatalf("CreateGuarded: %v", err)
	}
	wantData, _ := os.ReadFile(want)
	gotData, _ := os.ReadFile(got)
	if string(wantData) != string(gotData) || len(gotData) == 0 {
		t.Fatalf("CreateGuarded wrote %q, Create wrote %q", gotData, wantData)
	}
	if fi, err := os.Stat(got); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("created file mode = %v, err = %v; want 0600", fi.Mode().Perm(), err)
	}
	// An existing file is ErrExists, as for Create.
	if _, err := CreateGuarded(filepath.Join(outside, "guarded", "deep", ".tether"), "a", p, []string{catalog}); !errors.Is(err, ErrExists) {
		t.Fatalf("second create = %v; want ErrExists", err)
	}
	catalogUntouched(t, catalog)
}

// A destination in a protected directory is refused however it is reached, and
// nothing is created there, not even a directory.
func TestCreateGuarded_RefusesProtectedDestinations(t *testing.T) {
	catalog, outside := guardLayout(t)
	link := func(name, target string) string {
		p := filepath.Join(outside, name)
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if err := os.Mkdir(filepath.Join(outside, "layer"), 0o750); err != nil {
		t.Fatal(err)
	}
	link("layer/agents", filepath.Join(catalog, "agents"))
	for name, root := range map[string]string{
		"the catalog itself":                catalog,
		"a symlink to the catalog":          link("tether", catalog),
		"a symlink chain to the catalog":    link("chain", filepath.Join(outside, "tether")),
		"a subdirectory that is missing":    filepath.Join(catalog, "sub", "deeper"),
		"an agents/ symlinked in":           filepath.Join(outside, "layer"),
		"a dotdot back into the catalog":    filepath.Join(outside, "..", "catalog"),
		"a missing child of a symlinked in": filepath.Join(link("tether2", catalog), "new", "layer"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := CreateGuarded(root, "evil", Params{Name: "E"}, []string{catalog})
			if !errors.Is(err, ErrProtected) {
				t.Fatalf("CreateGuarded(%s) = %v; want ErrProtected", root, err)
			}
			catalogUntouched(t, catalog)
			if _, err := os.Stat(filepath.Join(catalog, "sub")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("a directory was made in the catalog (stat err = %v)", err)
			}
		})
	}
}

// The final component is never followed: a file that is a symlink into the
// catalog is neither created over nor written through.
func TestGuarded_NeverWritesThroughAFinalSymlink(t *testing.T) {
	catalog, outside := guardLayout(t)
	root := filepath.Join(outside, ".tether")
	if err := os.MkdirAll(filepath.Join(root, "agents"), 0o750); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(catalog, "agents", "keep.yaml")
	if err := os.Symlink(target, filepath.Join(root, "agents", "linked.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(catalog, "agents", "new.yaml"), filepath.Join(root, "agents", "dangling.yaml")); err != nil {
		t.Fatal(err)
	}

	if _, err := CreateGuarded(root, "linked", Params{Name: "X"}, []string{catalog}); !errors.Is(err, ErrExists) {
		t.Fatalf("create over a symlink = %v; want ErrExists", err)
	}
	if _, err := CreateGuarded(root, "dangling", Params{Name: "X"}, []string{catalog}); !errors.Is(err, ErrExists) {
		t.Fatalf("create over a dangling symlink = %v; want ErrExists", err)
	}
	if _, err := UpdateGuarded(filepath.Join(root, "agents", "linked.yaml"), Params{Name: "pwned"}, []string{catalog}); err == nil {
		t.Fatal("update through a symlink succeeded")
	}
	catalogUntouched(t, catalog)
}

func TestUpdateGuarded(t *testing.T) {
	catalog, outside := guardLayout(t)
	root := filepath.Join(outside, ".tether")
	if _, err := Create(root, "a", Params{Name: "Old", Roles: []string{"r"}}); err != nil {
		t.Fatal(err)
	}
	path := PathFor(root, "a")
	got, err := UpdateGuarded(path, Params{Name: "New"}, []string{catalog})
	if err != nil || got.Name != "New" || len(got.Roles) != 1 {
		t.Fatalf("UpdateGuarded = %+v, %v; want the same patch Update applies", got, err)
	}
	if data, _ := os.ReadFile(path); len(data) == 0 {
		t.Fatal("the file was truncated and not rewritten")
	}

	if _, err := UpdateGuarded(filepath.Join(root, "agents", "missing.yaml"), Params{Name: "x"}, []string{catalog}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("update of a missing file = %v; want not-exist", err)
	}

	// A protected file is refused and unchanged, directly and through a symlinked
	// directory.
	for name, p := range map[string]string{
		"directly":   filepath.Join(catalog, "agents", "keep.yaml"),
		"via a link": filepath.Join(outside, "viacat", "agents", "keep.yaml"),
	} {
		if name == "via a link" {
			if err := os.Symlink(catalog, filepath.Join(outside, "viacat")); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := UpdateGuarded(p, Params{Name: "pwned"}, []string{catalog}); !errors.Is(err, ErrProtected) {
			t.Fatalf("%s: UpdateGuarded = %v; want ErrProtected", name, err)
		}
	}
	catalogUntouched(t, catalog)

	// With no protected directories it is Update, and follows symlinks as it did.
	if _, err := UpdateGuarded(filepath.Join(outside, "viacat", "agents", "keep.yaml"), Params{Name: "Edited"}, nil); err != nil {
		t.Fatalf("unguarded update: %v", err)
	}
}

// The race the review reproduced: a path judged and then written loses to a
// symlink re-pointed in between (20000 project-scope creates with the layer
// root flipped between two symlinks, one into the catalog, wrote there 3861
// times). CreateGuarded and UpdateGuarded judge the directory they hold open, so
// the catalog is never written however fast the symlink flips.
func TestGuarded_SymlinkFlipNeverWritesTheCatalog(t *testing.T) {
	iterations := 3000
	if testing.Short() {
		iterations = 500
	}
	catalog, outside := guardLayout(t)
	safe := filepath.Join(outside, "safe")
	if err := os.MkdirAll(filepath.Join(safe, "agents"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(safe, "agents", "victim.yaml"), []byte("id: victim\nname: V\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// An agent of the same name in the catalog, which an edit must not reach.
	if err := os.WriteFile(filepath.Join(catalog, "agents", "victim.yaml"), []byte("id: victim\nname: V\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(outside, "repo-tether")
	flip := func(target string) {
		tmp := link + ".new"
		_ = os.Remove(tmp)
		if err := os.Symlink(target, tmp); err == nil {
			_ = os.Rename(tmp, link)
		}
	}
	flip(safe)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%2 == 0 {
				flip(catalog)
			} else {
				flip(safe)
			}
		}
	}()

	var created, updated, refused int
	for i := 0; i < iterations; i++ {
		if _, err := CreateGuarded(link, fmt.Sprintf("race-%d", i), Params{Name: "R"}, []string{catalog}); err == nil {
			created++
		} else if errors.Is(err, ErrProtected) {
			refused++
		}
		if _, err := UpdateGuarded(filepath.Join(link, "agents", "victim.yaml"), Params{Name: fmt.Sprintf("edited-%d", i)}, []string{catalog}); err == nil {
			updated++
		}
	}
	close(stop)
	wg.Wait()

	entries, err := os.ReadDir(filepath.Join(catalog, "agents"))
	if err != nil {
		t.Fatal(err)
	}
	var leaked []string
	for _, e := range entries {
		if e.Name() != "keep.yaml" && e.Name() != "victim.yaml" {
			leaked = append(leaked, e.Name())
		}
	}
	if data, _ := os.ReadFile(filepath.Join(catalog, "agents", "victim.yaml")); string(data) != "id: victim\nname: V\n" {
		t.Fatalf("an edit reached the catalog's agent: %q", data)
	}
	if len(leaked) > 0 {
		t.Fatalf("%d of %d creates wrote into the catalog: %v", len(leaked), iterations, leaked)
	}
	t.Logf("%d iterations: %d creates landed outside, %d refused, %d edits landed outside, none in the catalog", iterations, created, refused, updated)
	if created == 0 {
		t.Fatal("no create ever landed outside the catalog, so the test did not exercise the flip")
	}
}
