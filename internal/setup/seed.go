package setup

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// WriteOpts controls WriteCatalog behavior.
type WriteOpts struct {
	// Force overwrites existing files, backing each up as <path>.bak-<stamp>.
	Force bool
	// Minimal writes only global.yaml + the claude-code provider — the
	// smallest set the daemon can boot from. Used by the daemon auto-seed path.
	Minimal bool
	// ProviderCommands stamps detected binary paths into the seeded provider
	// YAMLs, keyed by brand ("claude", "codex", "opencode"). An empty value
	// for a brand leaves the provider YAML unchanged (command stays blank).
	ProviderCommands map[string]string
}

// WriteReport summarizes what WriteCatalog did.
type WriteReport struct {
	Written []string // relative paths written
	Skipped []string // relative paths skipped (already existed, Force false)
	Backed  []string // backup paths created (Force true)
}

// WriteCatalog writes the embedded starter catalog to dst, which is treated
// as the Tether state root (e.g. ~/.tether/). It creates:
//
//	<dst>/catalog/{global.yaml,providers/,mcp-servers/,...}
//	<dst>/state/
//	<dst>/run/
//	<dst>/logs/
//
// Existing files are skipped unless opts.Force is true, in which case each
// overwritten file is backed up as <path>.bak-<stamp> first.
//
// opts.ProviderCommands stamps detected binary paths into provider YAMLs so
// new installs reference the right binaries without manual editing.
func WriteCatalog(dst string, opts WriteOpts) (WriteReport, error) {
	var rep WriteReport

	// Create the state-root subdirectories the daemon expects.
	for _, sub := range []string{
		filepath.Join("catalog", "providers"),
		filepath.Join("catalog", "mcp-servers"),
		filepath.Join("catalog", "agents"),
		filepath.Join("catalog", "projects"),
		filepath.Join("catalog", "launches"),
		filepath.Join("catalog", "boot"),
		"state",
		"run",
		"logs",
	} {
		if err := os.MkdirAll(filepath.Join(dst, sub), 0o750); err != nil {
			return rep, fmt.Errorf("mkdir %s: %w", sub, err)
		}
	}

	// Walk the embedded seed tree and write files.
	err := fs.WalkDir(seedFS, "seed/catalog", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		// seed/catalog/... → catalog/...
		rel := strings.TrimPrefix(path, "seed/")
		dstPath := filepath.Join(dst, rel)

		if opts.Minimal && !isMinimalFile(rel) {
			return nil
		}

		data, readErr := seedFS.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("read embedded %s: %w", path, readErr)
		}

		// Apply ProviderCommands stamp if applicable.
		data = maybeStampCommand(data, rel, opts.ProviderCommands)

		written, backed, writeErr := writeFileOnce(dstPath, data, opts.Force)
		if writeErr != nil {
			return fmt.Errorf("write %s: %w", rel, writeErr)
		}
		if backed != "" {
			rep.Backed = append(rep.Backed, backed)
		}
		if written {
			rep.Written = append(rep.Written, rel)
		} else {
			rep.Skipped = append(rep.Skipped, rel)
		}
		return nil
	})
	if err != nil {
		return rep, err
	}

	return rep, nil
}

// isMinimalFile returns true for the files included in a Minimal write.
// A minimal catalog is: global.yaml + the claude-code provider.
func isMinimalFile(rel string) bool {
	switch rel {
	case filepath.Join("catalog", "global.yaml"):
		return true
	case filepath.Join("catalog", "providers", "claude-code.yaml"):
		return true
	}
	return false
}

// writeFileOnce writes data to path, backing up any existing file when force
// is true. Returns (written, backupPath, error). written is false when the
// file already existed and force was false.
func writeFileOnce(path string, data []byte, force bool) (written bool, backup string, err error) {
	if _, statErr := os.Stat(path); statErr == nil {
		// File exists.
		if !force {
			return false, "", nil
		}
		// Back it up.
		backup = uniqueBackupPath(path)
		if copyErr := copyFile(path, backup); copyErr != nil {
			return false, "", fmt.Errorf("backup %s: %w", path, copyErr)
		}
	}
	if writeErr := os.WriteFile(path, data, 0o640); writeErr != nil { //nolint:gosec // G306: catalog files are user-owned config
		return false, backup, writeErr
	}
	return true, backup, nil
}

// uniqueBackupPath returns a unique backup path in the form
// <path>.bak-<stamp>, matching the sysop backup convention.
func uniqueBackupPath(path string) string {
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	base := path + ".bak-" + stamp
	if _, err := os.Stat(base); os.IsNotExist(err) {
		return base
	}
	for i := 1; ; i++ {
		candidate := fmt.Sprintf("%s.%d", base, i)
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src) //nolint:gosec // G304: copying user-owned catalog file
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o640) //nolint:gosec // G306: catalog files are user-owned config
}

// maybeStampCommand patches the `command:` line in a provider YAML with the
// detected binary path from opts.ProviderCommands, keyed by brand. It reads
// the `provider:` field from the YAML to determine the brand.
func maybeStampCommand(data []byte, rel string, cmds map[string]string) []byte {
	if len(cmds) == 0 {
		return data
	}
	if !strings.HasPrefix(rel, filepath.Join("catalog", "providers")+string(filepath.Separator)) {
		return data
	}

	// Extract the brand from the YAML (minimal parse — only the provider field).
	var stub struct {
		Provider string `yaml:"provider"`
	}
	if yaml.Unmarshal(data, &stub) != nil || stub.Provider == "" {
		return data // not a provider YAML we know how to stamp
	}
	cmd, ok := cmds[stub.Provider]
	if !ok || cmd == "" {
		return data
	}

	return stampCommandLine(data, cmd)
}

// stampCommandLine replaces the `command:` line in a YAML byte slice with the
// given binary path, preserving indentation and surrounding content.
func stampCommandLine(data []byte, cmd string) []byte {
	lines := bytes.Split(data, []byte("\n"))
	for i, line := range lines {
		trimmed := strings.TrimSpace(string(line))
		if !strings.HasPrefix(trimmed, "command:") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(string(line), " \t"))
		lines[i] = []byte(strings.Repeat(" ", indent) + "command: " + cmd)
		break
	}
	return bytes.Join(lines, []byte("\n"))
}
