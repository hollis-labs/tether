package service

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

type archiveMember struct {
	name string
	kind byte
	link string
	body string
}

func fixtureArchive(t *testing.T, version string, members ...archiveMember) (string, string) {
	t.Helper()
	var data bytes.Buffer
	gz := gzip.NewWriter(&data)
	tw := tar.NewWriter(gz)
	for _, member := range members {
		kind := member.kind
		if kind == 0 {
			kind = tar.TypeReg
		}
		header := &tar.Header{Name: member.name, Typeflag: kind, Linkname: member.link, Mode: 0755}
		if kind == tar.TypeReg {
			header.Size = int64(len(member.body))
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if kind == tar.TypeReg {
			if _, err := tw.Write([]byte(member.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return fixtureArchiveBytes(t, version, data.Bytes())
}
func fixtureArchiveBytes(t *testing.T, version string, data []byte) (string, string) {
	t.Helper()
	dir := t.TempDir()
	archive := filepath.Join(dir, archiveName(version))
	if err := os.WriteFile(archive, data, 0600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	checksums := filepath.Join(dir, "checksums.txt")
	if err := os.WriteFile(checksums, []byte(hex.EncodeToString(hash[:])+"  "+archiveName(version)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return archive, checksums
}
func testRuntime(t *testing.T) Runtime {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("Linux runtime")
	}
	return Runtime{Root: filepath.Join(t.TempDir(), "runtime")}
}
func installedFixture(t *testing.T, r Runtime, version string) {
	t.Helper()
	archive, checksums := fixtureArchive(t, version, archiveMember{name: "tether", body: "synthetic executable " + version})
	if _, err := r.Install(context.Background(), version, archive, checksums); err != nil {
		t.Fatal(err)
	}
}

func TestExactRuntimeVersion(t *testing.T) {
	for _, version := range []string{"0.8.0", "1.2.3-alpha.1", "1.2.3+build.01", "1.2.3-beta+build"} {
		if err := ValidateVersion(version); err != nil {
			t.Errorf("%s: %v", version, err)
		}
	}
	for _, version := range []string{"latest", "v0.8.0", "0.8", "01.2.3", "1.2.3-01", "1.2.3-a..b", "../1.2.3", "1.2.3/other", "^1.2.3", ">=1.2.3", "1.2.3\n", "1.2.3;id"} {
		if err := ValidateVersion(version); err == nil {
			t.Errorf("accepted %q", version)
		}
	}
}

func TestRuntimeInstallAndSwitchBack(t *testing.T) {
	r := testRuntime(t)
	installedFixture(t, r, "0.8.0")
	if err := r.Select("0.8.0"); err != nil {
		t.Fatal(err)
	}
	installedFixture(t, r, "0.8.1")
	if err := r.Select("0.8.1"); err != nil {
		t.Fatal(err)
	}
	current, err := r.Selector("current")
	if err != nil || current != "0.8.1" {
		t.Fatalf("current %s %v", current, err)
	}
	previous, err := r.Selector("previous")
	if err != nil || previous != "0.8.0" {
		t.Fatalf("previous %s %v", previous, err)
	}
	if err := r.Select(previous); err != nil {
		t.Fatal(err)
	}
	if current, _ := r.Selector("current"); current != "0.8.0" {
		t.Fatal("switch-back did not select retained runtime")
	}
	if _, err := r.Ready("0.8.1"); err != nil {
		t.Fatal("previous runtime was not retained", err)
	}
}

func TestRuntimeRejectsArchiveHazards(t *testing.T) {
	tests := map[string][]archiveMember{
		"escape":              {{name: "../sentinel", body: "bad"}},
		"absolute":            {{name: "/sentinel", body: "bad"}},
		"symlink":             {{name: "tether", kind: tar.TypeSymlink, link: "../sentinel"}},
		"hardlink":            {{name: "tether", kind: tar.TypeLink, link: "../sentinel"}},
		"fifo":                {{name: "tether", kind: tar.TypeFifo}},
		"device":              {{name: "tether", kind: tar.TypeChar}},
		"duplicate":           {{name: "tether", body: "one"}, {name: "tether", body: "two"}},
		"canonical-duplicate": {{name: "tether", body: "one"}, {name: "./tether", body: "two"}},
		"reserved-marker":     {{name: "tether", body: "one"}, {name: ".install-complete", body: "0.8.0"}},
		"reserved-archive":    {{name: "tether", body: "one"}, {name: ".archive.tar.gz", body: "bad"}},
		"missing-binary":      {{name: "another", body: "missing"}},
		"parent-file":         {{name: "sub", body: "file"}, {name: "sub/tether", body: "nested"}},
	}
	for name, members := range tests {
		t.Run(name, func(t *testing.T) {
			r := testRuntime(t)
			sentinel := filepath.Join(filepath.Dir(r.Root), "sentinel")
			if err := os.WriteFile(sentinel, []byte("untouched"), 0600); err != nil {
				t.Fatal(err)
			}
			archive, checksums := fixtureArchive(t, "0.8.0", members...)
			if _, err := r.Install(context.Background(), "0.8.0", archive, checksums); err == nil {
				t.Fatal("unsafe archive installed")
			}
			if _, err := os.Lstat(filepath.Join(r.Root, "versions", "0.8.0")); !os.IsNotExist(err) {
				t.Fatal("failed archive published", err)
			}
			data, err := os.ReadFile(sentinel)
			if err != nil || string(data) != "untouched" {
				t.Fatal("archive escaped owned staging")
			}
		})
	}
}

func TestRuntimeChecksumAndIncompleteRefusal(t *testing.T) {
	r := testRuntime(t)
	archive, checksums := fixtureArchive(t, "0.8.0", archiveMember{name: "tether", body: "fixture"})
	if err := os.WriteFile(checksums, []byte(strings.Repeat("0", 64)+"  "+archiveName("0.8.0")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Install(context.Background(), "0.8.0", archive, checksums); err == nil || !strings.Contains(err.Error(), "checksum-mismatch") {
		t.Fatal("checksum mismatch not refused", err)
	}
	archive, checksums = fixtureArchive(t, "0.8.0", archiveMember{name: "tether", body: "fixture"})
	if err := os.MkdirAll(filepath.Join(r.Root, "versions", "0.8.0"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Install(context.Background(), "0.8.0", archive, checksums); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatal("incomplete existing runtime overwritten", err)
	}
}

func TestRuntimeRejectsTruncatedAndCorruptGzip(t *testing.T) {
	for _, damaged := range []string{"truncated", "trailer"} {
		t.Run(damaged, func(t *testing.T) {
			r := testRuntime(t)
			archive, _ := fixtureArchive(t, "0.8.0", archiveMember{name: "tether", body: "fixture"})
			data, err := os.ReadFile(archive)
			if err != nil {
				t.Fatal(err)
			}
			if damaged == "truncated" {
				data = data[:len(data)-4]
			} else {
				data[len(data)-8] ^= 0xff
			}
			archive, checksums := fixtureArchiveBytes(t, "0.8.0", data)
			if _, err := r.Install(context.Background(), "0.8.0", archive, checksums); err == nil {
				t.Fatal("incomplete gzip published")
			}
		})
	}
}

func TestRuntimeConcurrentInstallAndImmutability(t *testing.T) {
	r := testRuntime(t)
	archive, checksums := fixtureArchive(t, "0.8.0", archiveMember{name: "tether", body: "fixture"})
	var wg sync.WaitGroup
	errors := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.Install(context.Background(), "0.8.0", archive, checksums)
			errors <- err
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	changed, newChecksums := fixtureArchive(t, "0.8.0", archiveMember{name: "tether", body: "changed"})
	if _, err := r.Install(context.Background(), "0.8.0", changed, newChecksums); err == nil {
		t.Fatal("immutable version replaced")
	}
	if err := os.WriteFile(filepath.Join(r.Root, "versions", "0.8.0", "tether"), []byte("tampered"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Ready("0.8.0"); err == nil {
		t.Fatal("changed executable provenance accepted")
	}
}

func TestRuntimeChecksumAmbiguityAndSelectorEscape(t *testing.T) {
	r := testRuntime(t)
	archive, checksums := fixtureArchive(t, "0.8.0", archiveMember{name: "tether", body: "fixture"})
	data, err := os.ReadFile(checksums)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(checksums, append(data, data...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Install(context.Background(), "0.8.0", archive, checksums); err == nil {
		t.Fatal("duplicate checksum entries accepted")
	}
	if err := r.prepare(); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../outside", filepath.Join(r.Root, "current")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Selector("current"); err == nil {
		t.Fatal("selector escaped versions")
	}
}

func TestRuntimeCancellationBeforePublish(t *testing.T) {
	r := testRuntime(t)
	archive, checksums := fixtureArchive(t, "0.8.0", archiveMember{name: "tether", body: "fixture"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Install(ctx, "0.8.0", archive, checksums); err == nil {
		t.Fatal("canceled install published")
	}
}
