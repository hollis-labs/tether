package antigravity

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

func TestPrependPATH(t *testing.T) {
	sep := string(os.PathListSeparator)
	cases := []struct {
		name string
		env  []string
		want []string
	}{
		{"prepends and keeps the rest", []string{"HOME=/h", "PATH=/usr/bin" + sep + "/bin"}, []string{"HOME=/h", "PATH=/shim" + sep + "/usr/bin" + sep + "/bin"}},
		{"no PATH sets it", []string{"HOME=/h"}, []string{"HOME=/h", "PATH=/shim"}},
		{"empty PATH", []string{"PATH="}, []string{"PATH=/shim"}},
		{"every duplicate rewritten", []string{"PATH=/a", "PATH=/b"}, []string{"PATH=/shim" + sep + "/a", "PATH=/shim" + sep + "/b"}},
		{"similar names untouched", []string{"MANPATH=/m", "PATHX=/x", "PATH=/a"}, []string{"MANPATH=/m", "PATHX=/x", "PATH=/shim" + sep + "/a"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := slices.Clone(tc.env)
			if got := PrependPATH(tc.env, "/shim"); !slices.Equal(got, tc.want) {
				t.Errorf("PrependPATH = %q; want %q", got, tc.want)
			}
			if !slices.Equal(tc.env, in) {
				t.Errorf("input env mutated: %q", tc.env)
			}
		})
	}
}

func TestPlantBrowserShim_Contents(t *testing.T) {
	dir := filepath.Join(t.TempDir(), BrowserShimDirName)
	got, err := PlantBrowserShim(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != dir {
		t.Errorf("dir = %q; want %q", got, dir)
	}
	for _, name := range []string{"open", "xdg-open"} {
		p := filepath.Join(dir, name)
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != browserShimScript {
			t.Errorf("%s = %q", name, b)
		}
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s not executable: %v", name, fi.Mode())
		}
	}
	// Planting twice (a relaunch into the same workspace) is fine.
	if _, err := PlantBrowserShim(dir); err != nil {
		t.Fatalf("second plant: %v", err)
	}
}

// An opener looked up on PATH, as agy's is, resolves to the shim and fails.
// `open -h` only prints usage, so a shim that failed to shadow the real
// opener would still show nothing on screen.
func TestPlantBrowserShim_ShadowsOpenerOnPATH(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shim is a sh script")
	}
	dir, err := PlantBrowserShim(filepath.Join(t.TempDir(), BrowserShimDirName))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"open", "xdg-open"} {
		cmd := exec.Command("/bin/sh", "-c", name+" -h")
		cmd.Env = PrependPATH([]string{"PATH=" + os.Getenv("PATH")}, dir)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		err := cmd.Run()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			t.Errorf("%s: err = %v; want exit 1", name, err)
		}
		if got := stderr.String(); got != "tether: browser open suppressed\n" {
			t.Errorf("%s stderr = %q", name, got)
		}
	}
}
