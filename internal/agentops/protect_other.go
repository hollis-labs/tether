//go:build !linux

package agentops

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launchprofile"
)

// refuseResolved refuses a path that, resolved through symlinks, lies in a
// protected directory. Off Linux this is the whole check, so a symlink
// re-pointed between it and the write defeats it: narrowed, not closed. The
// protection is not applied off Linux yet (CW-20261001-0138).
func refuseResolved(path string, protected []string) error {
	resolved := config.RealPath(path)
	for _, dir := range protected {
		if config.PathWithin(dir, resolved) {
			return fmt.Errorf("%w: %s", ErrProtected, path)
		}
	}
	return nil
}

func createFileGuarded(path string, body []byte, protected []string) error {
	if err := refuseResolved(path, protected); err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%w at %s", ErrExists, path)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o600)
}

func updateFileGuarded(path string, p Params, protected []string) (launchprofile.LaunchProfile, error) {
	if err := refuseResolved(path, protected); err != nil {
		return launchprofile.LaunchProfile{}, err
	}
	return Update(path, p)
}
