// Package service owns only the explicitly managed worker service and its
// immutable runtimes (ADR 0064). It never adopts another supervisor's daemon.
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

var exactVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$`)

func ValidateVersion(version string) error {
	match := exactVersion.FindStringSubmatch(version)
	if match == nil {
		return fmt.Errorf("%q is not an exact SemVer (for example 0.8.0)", version)
	}
	for _, id := range strings.Split(match[4], ".") {
		numeric := id != ""
		for _, c := range id {
			numeric = numeric && c >= '0' && c <= '9'
		}
		if numeric && len(id) > 1 && id[0] == '0' {
			return fmt.Errorf("numeric prerelease identifiers cannot have leading zeroes")
		}
	}
	return nil
}

type Runtime struct{ Root string }

type InstallRecord struct {
	Version      string `json:"version"`
	Archive      string `json:"archive"`
	SHA256       string `json:"sha256"`
	BinarySHA256 string `json:"binary_sha256"`
}

func (r Runtime) prepare() error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("worker service runtimes support Linux only")
	}
	if !filepath.IsAbs(r.Root) {
		return fmt.Errorf("runtime root must be absolute")
	}
	if err := ensureDirectory(r.Root); err != nil {
		return err
	}
	return ensureDirectory(filepath.Join(r.Root, "versions"))
}

func ensureDirectory(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("expected a real directory: %s", path)
	}
	return nil
}

func readRegular(path string, maxBytes int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxBytes {
		return nil, fmt.Errorf("invalid or oversized file: %s", path)
	}
	file, err := os.Open(path) //nolint:gosec // caller-selected input or owned provenance; regular identity checked
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) {
		return nil, fmt.Errorf("file identity changed: %s", path)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("file grew beyond bound: %s", path)
	}
	return data, nil
}

func (r Runtime) Ready(version string) (InstallRecord, error) {
	var record InstallRecord
	if err := ValidateVersion(version); err != nil {
		return record, err
	}
	dir := filepath.Join(r.Root, "versions", version)
	info, err := os.Lstat(dir)
	if err != nil {
		return record, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return record, fmt.Errorf("runtime directory is not an installed directory")
	}
	marker, err := readRegular(filepath.Join(dir, ".install-complete"), 512)
	if err != nil || string(marker) != version+"\n" {
		return record, fmt.Errorf("incomplete runtime %s", version)
	}
	data, err := readRegular(filepath.Join(dir, ".install.json"), 4096)
	if err != nil {
		return record, err
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return record, err
	}
	if record.Version != version || !sha256Hex.MatchString(record.SHA256) || !sha256Hex.MatchString(record.BinarySHA256) || record.Archive != archiveName(version) {
		return record, fmt.Errorf("invalid runtime provenance for %s", version)
	}
	binary, err := os.Lstat(filepath.Join(dir, "tether"))
	if err != nil {
		return record, err
	}
	if !binary.Mode().IsRegular() || binary.Mode().Perm()&0111 == 0 {
		return record, fmt.Errorf("runtime lacks an executable tether binary")
	}
	digest, err := fileSHA256(filepath.Join(dir, "tether"))
	if err != nil {
		return record, err
	}
	if digest != record.BinarySHA256 {
		return record, fmt.Errorf("runtime executable provenance changed")
	}
	return record, nil
}

func archiveName(version string) string {
	return "tether_" + version + "_linux_" + runtime.GOARCH + ".tar.gz"
}

func (r Runtime) Selector(name string) (string, error) {
	if name != "current" && name != "previous" {
		return "", fmt.Errorf("invalid runtime selector")
	}
	target, err := os.Readlink(filepath.Join(r.Root, name))
	if err != nil {
		return "", err
	}
	version := strings.TrimPrefix(target, "versions/")
	if target != "versions/"+version {
		return "", fmt.Errorf("runtime selector escapes versions")
	}
	if _, err := r.Ready(version); err != nil {
		return "", err
	}
	return version, nil
}

// Select is called under the manager's operation lock. Both links are replaced
// atomically; previous is published first, so a crash never loses switch-back.
func (r Runtime) Select(version string) error {
	if _, err := r.Ready(version); err != nil {
		return err
	}
	previous, err := r.Selector("current")
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if previous == version {
		return nil
	}
	if previous != "" {
		if err := r.replaceLink("previous", previous); err != nil {
			return err
		}
	}
	return r.replaceLink("current", version)
}

func (r Runtime) replaceLink(name, version string) error {
	if info, err := os.Lstat(filepath.Join(r.Root, name)); err == nil && info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("refusing to replace non-symlink %s", name)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	token, err := randomToken()
	if err != nil {
		return err
	}
	temp := filepath.Join(r.Root, "."+name+"-"+token)
	if err := os.Symlink("versions/"+version, temp); err != nil {
		return err
	}
	defer func() { _ = os.Remove(temp) }()
	return os.Rename(temp, filepath.Join(r.Root, name))
}

func writeAtomic(path string, data []byte) error {
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("refusing to replace non-regular file %s", path)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".write-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if err := file.Chmod(0600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func (r Runtime) Install(ctx context.Context, version, archive, checksums string) (InstallRecord, error) {
	var record InstallRecord
	if err := ValidateVersion(version); err != nil {
		return record, err
	}
	if filepath.Base(archive) != archiveName(version) {
		return record, fmt.Errorf("archive must be named %s", archiveName(version))
	}
	expected, err := releaseChecksum(checksums, archiveName(version))
	if err != nil {
		return record, err
	}
	if err := r.prepare(); err != nil {
		return record, err
	}
	unlock, err := acquireLock(ctx, filepath.Join(r.Root, ".install.lock"))
	if err != nil {
		return record, err
	}
	defer unlock()
	stage, err := os.MkdirTemp(filepath.Join(r.Root, "versions"), ".staging-")
	if err != nil {
		return record, err
	}
	defer func() { _ = os.RemoveAll(stage) }() // only this invocation's unpublished staging dir
	stagedArchive := filepath.Join(stage, ".archive.tar.gz")
	digest, err := copyArchive(ctx, archive, stagedArchive)
	if err != nil {
		return record, err
	}
	if digest != expected {
		return record, fmt.Errorf("checksum-mismatch: %s", archiveName(version))
	}
	if ready, err := r.Ready(version); err == nil {
		if ready.SHA256 != digest {
			return record, fmt.Errorf("immutable runtime %s has different provenance", version)
		}
		return ready, nil
	}
	destination := filepath.Join(r.Root, "versions", version)
	if _, err := os.Lstat(destination); err == nil {
		return record, fmt.Errorf("refusing to overwrite incomplete runtime %s", version)
	} else if !os.IsNotExist(err) {
		return record, err
	}
	if err := extractArchive(ctx, stagedArchive, stage); err != nil {
		return record, err
	}
	if err := os.Remove(stagedArchive); err != nil {
		return record, err
	}
	binary, err := os.Lstat(filepath.Join(stage, "tether"))
	if err != nil || !binary.Mode().IsRegular() {
		return record, fmt.Errorf("archive lacks a regular root tether binary")
	}
	if err := os.Chmod(filepath.Join(stage, "tether"), 0700); err != nil { //nolint:gosec // user executable inside owned 0700 staging
		return record, err
	}
	record = InstallRecord{Version: version, Archive: archiveName(version), SHA256: digest}
	record.BinarySHA256, err = fileSHA256(filepath.Join(stage, "tether"))
	if err != nil {
		return record, err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return record, err
	}
	if err := writeAtomic(filepath.Join(stage, ".install.json"), data); err != nil {
		return record, err
	}
	if err := writeAtomic(filepath.Join(stage, ".install-complete"), []byte(version+"\n")); err != nil {
		return record, err
	}
	if err := ctx.Err(); err != nil {
		return record, err
	}
	if err := os.Rename(stage, destination); err != nil {
		return record, err
	}
	return record, nil
}
