package workspace

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// CopyNativeState copies provider session history into an already planted home.
// The caller must validate both homes against canonical session metadata and
// call this before starting the provider. Only Codex sessions and Claude
// projects are copied; config, credentials, and other home files are untouched.
// Existing destination state is never overwritten. Missing source state and
// symlinks are errors. A failed copy may leave partial destination state; abort
// that launch rather than starting with it or retrying over it.
// Replanting the same home preserves its existing state without copying.
func CopyNativeState(sourceProviderHome, targetProviderHome, providerBrand string) error {
	var stateDir string
	switch providerBrand {
	case "codex":
		stateDir = "sessions"
	case "claude":
		stateDir = "projects"
	default:
		return fmt.Errorf("native state: unsupported provider brand %q", providerBrand)
	}
	if !filepath.IsAbs(sourceProviderHome) || !filepath.IsAbs(targetProviderHome) {
		return fmt.Errorf("native state: provider homes must be absolute")
	}
	sourceInfo, err := os.Lstat(sourceProviderHome)
	if err != nil {
		return fmt.Errorf("native state: source home: %w", err)
	}
	targetInfo, err := os.Lstat(targetProviderHome)
	if err != nil {
		return fmt.Errorf("native state: target home: %w", err)
	}
	if !sourceInfo.IsDir() || !targetInfo.IsDir() {
		return fmt.Errorf("native state: provider homes must be real directories")
	}
	if os.SameFile(sourceInfo, targetInfo) {
		return nil
	}
	sourceHome, err := os.OpenRoot(sourceProviderHome)
	if err != nil {
		return fmt.Errorf("native state: open source home: %w", err)
	}
	defer func() { _ = sourceHome.Close() }()
	stateInfo, err := sourceHome.Lstat(stateDir)
	if err != nil {
		return fmt.Errorf("native state: source %s: %w", stateDir, err)
	}
	if !stateInfo.IsDir() {
		return fmt.Errorf("native state: source %s must be a real directory", stateDir)
	}
	source, err := sourceHome.OpenRoot(stateDir)
	if err != nil {
		return fmt.Errorf("native state: open source %s: %w", stateDir, err)
	}
	defer func() { _ = source.Close() }()

	// Validate the complete state tree before creating anything in the target.
	// Root-confined reads also prevent a concurrent link swap from reaching
	// config or credentials outside the state directory.
	type stateEntry struct {
		path string
		dir  bool
	}
	var entries []stateEntry
	err = fs.WalkDir(source.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == "." {
			return nil
		}
		info, statErr := source.Lstat(path)
		if statErr != nil {
			return statErr
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported state entry %q", path)
		}
		entries = append(entries, stateEntry{path: path, dir: info.IsDir()})
		return nil
	})
	if err != nil {
		return fmt.Errorf("native state: validate source %s: %w", stateDir, err)
	}
	target, err := os.OpenRoot(targetProviderHome)
	if err != nil {
		return fmt.Errorf("native state: open target home: %w", err)
	}
	defer func() { _ = target.Close() }()
	if err := target.Mkdir(stateDir, 0o700); err != nil {
		return fmt.Errorf("native state: create target %s without overwriting: %w", stateDir, err)
	}
	for _, entry := range entries {
		destination := filepath.Join(stateDir, entry.path)
		if entry.dir {
			if err := target.Mkdir(destination, 0o700); err != nil {
				return fmt.Errorf("native state: create state directory: %w", err)
			}
			continue
		}
		if err := copyNativeStateFile(source, target, entry.path, destination); err != nil {
			return fmt.Errorf("native state: copy %q: %w", entry.path, err)
		}
	}
	return nil
}

func copyNativeStateFile(source, target *os.Root, sourcePath, targetPath string) error {
	input, err := source.Open(sourcePath)
	if err != nil {
		return err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("source must be a regular file")
	}
	output, err := target.OpenFile(targetPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}
