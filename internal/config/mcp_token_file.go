package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hollis-labs/tether/internal/credfile"
)

// StdioCredentialArgs validates an opt-in token_file at the spawn boundary.
// The returned secret is only for stderr redaction; it is never an argument.
func (e MCPServerEntry) StdioCredentialArgs() (args []string, redact string, err error) {
	args = append([]string(nil), e.Args...)
	if e.TokenFile == "" {
		return args, "", nil
	}
	if err := e.validateTokenFile(); err != nil {
		return nil, "", err
	}
	roots := []string{e.catalogDir}
	if home, err := os.UserHomeDir(); err == nil {
		roots = append(roots, home)
	}
	redact, err = credfile.Read(e.TokenFile, credfile.Options{Roots: roots})
	if err != nil {
		return nil, "", fmt.Errorf("token_file: %w", err)
	}
	for _, arg := range append(args, e.Command, e.TokenFile) {
		if strings.Contains(arg, redact) {
			return nil, "", fmt.Errorf("token_file credential must not appear in arguments or paths")
		}
	}
	path := e.TokenFile
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, "", err
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~"))
	}
	if strings.Contains(path, redact) {
		return nil, "", fmt.Errorf("token_file credential must not appear in paths")
	}
	return append(args, "--token-file", filepath.Clean(path)), redact, nil
}

func (e MCPServerEntry) validateTokenFile() error {
	if e.TokenFile == "" {
		return nil
	}
	if e.Transport != "" && e.Transport != "stdio" {
		return fmt.Errorf("token_file requires stdio transport")
	}
	if e.Token != "" {
		return fmt.Errorf("token_file cannot be combined with token")
	}
	for _, arg := range e.Args {
		if arg == "--token" || strings.HasPrefix(arg, "--token=") || arg == "--token-file" || strings.HasPrefix(arg, "--token-file=") {
			return fmt.Errorf("token_file cannot be combined with token arguments")
		}
	}
	return nil
}
