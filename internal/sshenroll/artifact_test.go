package sshenroll

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHubArtifactSnapshotAndChecksums(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			archive, checksums, original := testRelease(t, arch)
			a, err := VerifyArtifact(context.Background(), archive, checksums, "0.8.0", arch, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			if err := os.WriteFile(archive, []byte("changed after snapshot"), 0600); err != nil {
				t.Fatal(err)
			}
			reader, err := a.Reader()
			if err != nil {
				t.Fatal(err)
			}
			actual, err := io.ReadAll(reader)
			if err != nil || !bytes.Equal(actual, original) {
				t.Fatal("upload reread mutable input")
			}
			if string(a.Checksums()) != a.SHA256+"  "+a.Name+"\n" {
				t.Fatal("worker checksum snapshot differs")
			}
		})
	}
}
func TestHubChecksumFailurePreventsWorkerChanges(t *testing.T) {
	m, o, f := enrollmentFixture(t)
	if err := os.WriteFile(o.Checksums, []byte(strings.Repeat("0", 64)+"  "+filepath.Base(o.Archive)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Add(context.Background(), o); err == nil {
		t.Fatal("bad checksum accepted")
	}
	if f.installs != 0 || f.grants != 0 {
		t.Fatal("bad hub artifact changed worker")
	}
}
func TestHubArchiveRefusesEscapesLinksDuplicatesAndTruncation(t *testing.T) {
	for _, bad := range []struct {
		name string
		kind byte
	}{{"../escape", tar.TypeReg}, {"tether", tar.TypeSymlink}, {"tether", tar.TypeLink}, {"/escape", tar.TypeReg}, {".install-complete", tar.TypeReg}} {
		t.Run(bad.name+string(bad.kind), func(t *testing.T) {
			var b bytes.Buffer
			gz := gzip.NewWriter(&b)
			tw := tar.NewWriter(gz)
			h := &tar.Header{Name: bad.name, Mode: 0700, Typeflag: bad.kind, Linkname: "outside"}
			if bad.kind == tar.TypeReg {
				h.Size = 1
			}
			if err := tw.WriteHeader(h); err != nil {
				t.Fatal(err)
			}
			if h.Size > 0 {
				_, _ = tw.Write([]byte("x"))
			}
			_ = tw.Close()
			_ = gz.Close()
			if _, err := bootstrapBinary(b.Bytes()); err == nil {
				t.Fatal("unsafe archive accepted")
			}
		})
	}
	var duplicate bytes.Buffer
	gz := gzip.NewWriter(&duplicate)
	tw := tar.NewWriter(gz)
	for range 2 {
		if err := tw.WriteHeader(&tar.Header{Name: "tether", Mode: 0700, Typeflag: tar.TypeReg, Size: 1}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := bootstrapBinary(duplicate.Bytes()); err == nil {
		t.Fatal("duplicate executable accepted")
	}
	_, _, b := testRelease(t, "amd64")
	if _, err := bootstrapBinary(b[:len(b)-8]); err == nil {
		t.Fatal("missing gzip trailer accepted")
	}
}
