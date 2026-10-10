package sshenroll

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/hollis-labs/tether/internal/service"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const maxArchive int64 = 512 << 20

var digestPattern = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)

// Artifact is an owned snapshot verified once on the hub. Workers do not fetch
// releases, module credentials, or checksums from an external service.
type Artifact struct {
	Name, SHA256 string
	file         *os.File
	binary       []byte
}

func (a *Artifact) Close() error {
	path := a.file.Name()
	err := a.file.Close()
	_ = os.Remove(path)
	return err
}
func (a *Artifact) Reader() (io.Reader, error) {
	_, err := a.file.Seek(0, io.SeekStart)
	return a.file, err
}
func (a *Artifact) Checksums() []byte { return []byte(a.SHA256 + "  " + a.Name + "\n") }
func (a *Artifact) Binary() io.Reader { return bytes.NewReader(a.binary) }

func readInput(path string, byteLimit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > byteLimit {
		return nil, problem("artifact", "invalid-input", "Use regular bounded release files; symlinks and special files are refused.")
	}
	f, err := os.Open(path) //nolint:gosec // explicit release input, opened identity verified below
	if err != nil {
		return nil, problem("artifact", "unreadable-input", "Cannot open the explicit release input.")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, problem("artifact", "changed-input", "The release input changed while opening.")
	}
	b, err := io.ReadAll(io.LimitReader(f, byteLimit+1))
	if err != nil || int64(len(b)) > byteLimit {
		return nil, problem("artifact", "invalid-input", "The release input exceeds its byte limit.")
	}
	return b, nil
}

func VerifyArtifact(ctx context.Context, archive, checksums, version, arch, root string) (*Artifact, error) {
	if service.ValidateVersion(version) != nil {
		return nil, problem("artifact", "invalid-version", "Use an exact supported release version.")
	}
	if arch != "amd64" && arch != "arm64" {
		return nil, problem("artifact", "unsupported-architecture", "Only Linux amd64 and arm64 releases are supported.")
	}
	name := fmt.Sprintf("tether_%s_linux_%s.tar.gz", version, arch)
	if filepath.Base(archive) != name {
		return nil, problem("artifact", "wrong-archive", "Use the exact release archive for the preflight architecture.")
	}
	list, err := readInput(checksums, 1<<20)
	if err != nil {
		return nil, err
	}
	want := ""
	for _, line := range strings.Split(string(list), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != name {
			continue
		}
		if want != "" || !digestPattern.MatchString(fields[0]) {
			return nil, problem("artifact", "ambiguous-checksum", "Release checksums must contain exactly one valid entry for the archive.")
		}
		want = strings.ToLower(fields[0])
	}
	if want == "" {
		return nil, problem("artifact", "missing-checksum", "Release checksums have no exact archive entry.")
	}
	data, err := readInput(archive, maxArchive)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != want {
		return nil, problem("artifact", "checksum-mismatch", "Obtain matching release archive and checksums before changing the worker.")
	}
	binary, err := bootstrapBinary(data)
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(root, ".verified-archive-")
	if err != nil {
		return nil, problem("artifact", "snapshot-failed", "Cannot stage the verified archive privately on the hub.")
	}
	a := &Artifact{Name: name, SHA256: want, file: f, binary: binary}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if err != nil {
		_ = a.Close()
		return nil, problem("artifact", "snapshot-failed", "Cannot sync the verified hub archive.")
	}
	return a, nil
}

func bootstrapBinary(data []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, problem("artifact", "invalid-archive", "The verified release is not a readable gzip archive.")
	}
	defer func() { _ = gz.Close() }()
	reader := tar.NewReader(gz)
	var binary []byte
	seen := map[string]bool{}
	var total int64
	for count := 0; ; count++ {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || count >= 2048 {
			return nil, problem("artifact", "invalid-archive", "The release archive is incomplete or oversized.")
		}
		name := filepath.Clean(header.Name)
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") || strings.Contains(header.Name, "\\") || seen[name] || strings.HasPrefix(name, ".install") || strings.HasPrefix(name, ".archive") {
			return nil, problem("artifact", "unsafe-archive", "Archive paths or duplicates are unsafe.")
		}
		seen[name] = true
		if header.Typeflag == tar.TypeDir {
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != 0 {
			return nil, problem("artifact", "unsafe-archive", "Archive links and special files are refused.")
		}
		if header.Size < 0 || header.Size > 2<<30-total {
			return nil, problem("artifact", "oversized-archive", "The extracted release exceeds its byte limit.")
		}
		total += header.Size
		if name == "tether" {
			if header.Size > maxArchive || header.Mode&0111 == 0 {
				return nil, problem("artifact", "invalid-bootstrap", "Release must include an executable tether binary.")
			}
			binary, err = io.ReadAll(io.LimitReader(reader, maxArchive+1))
			if err != nil || int64(len(binary)) != header.Size {
				return nil, problem("artifact", "invalid-bootstrap", "Release bootstrap is truncated.")
			}
		}
	}
	if len(binary) == 0 {
		return nil, problem("artifact", "missing-bootstrap", "Release must include tether at the archive root.")
	}
	if n, err := io.Copy(io.Discard, io.LimitReader(gz, 2<<30-total+1)); err != nil || n > 2<<30-total {
		return nil, problem("artifact", "invalid-archive", "The release gzip trailer is incomplete.")
	}
	return binary, nil
}
