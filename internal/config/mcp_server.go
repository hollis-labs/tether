package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/hollis-labs/tether/internal/llm/secrets"
	"gopkg.in/yaml.v3"
)

// MCPServerEntry describes one upstream MCP server in the catalog.
// Files live at <catalogDir>/mcp-servers/*.yaml.
//
// Credential-bearing fields (Args, Env, Token, URL) accept a secret reference
// — keychain://<authority>/<path> or helper://<name>/<path> — in place of a
// literal value. References are resolved only by LoadMCPServers, at spawn
// time; see resolveEntrySecrets.
type MCPServerEntry struct {
	ID        string            `yaml:"id"`
	Transport string            `yaml:"transport"` // "stdio" | "sse"
	Command   string            `yaml:"command"`   // stdio: binary path
	Args      []string          `yaml:"args"`      // stdio: arguments; support ${VAR} and secret refs
	Env       map[string]string `yaml:"env"`       // env vars; values support ${VAR} and secret refs
	URL       string            `yaml:"url"`       // sse: endpoint URL
	Token     string            `yaml:"token"`     // bearer token, ${VAR} ref, or secret ref
	Scopes    []string          `yaml:"scopes"`
	Enabled   *bool             `yaml:"enabled"` // nil → defaults to true
	Tags      []string          `yaml:"tags"`
}

// IsEnabled returns true when the entry should be loaded. A missing enabled
// field (nil pointer) is treated as true.
func (e *MCPServerEntry) IsEnabled() bool {
	return e.Enabled == nil || *e.Enabled
}

var envVarRE = regexp.MustCompile(`\$\{([^}]+)\}`)

// expandEnvRefs replaces ${VAR} tokens with os.Getenv(VAR) values.
func expandEnvRefs(s string) string {
	return envVarRE.ReplaceAllStringFunc(s, func(match string) string {
		key := match[2 : len(match)-1] // strip ${ and }
		return os.Getenv(key)
	})
}

// secretRefTimeout bounds one helper invocation. Resolution shells out to
// mux-apikey-helper, which on macOS may block on a keychain ACL prompt.
const secretRefTimeout = 15 * time.Second

// secretRefResolver is the resolution surface used by resolveSecretRef.
// Package-level so tests can substitute a stub without touching the keychain.
type secretRefResolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

var secretResolver secretRefResolver = secrets.NewResolver()

// isSecretRef reports whether s carries a scheme that resolveSecretRef should
// hand to the resolver. Anything else is a literal and is passed through, so a
// catalog holding plain values keeps working unchanged.
func isSecretRef(s string) bool {
	return strings.HasPrefix(s, "keychain://") || strings.HasPrefix(s, "helper://")
}

// resolveSecretRef resolves s when it is a secret reference and returns it
// unchanged otherwise.
//
// The resolved secret is never logged and never reaches the returned error —
// only the reference and the field that carried it are named, both of which are
// non-sensitive by construction.
func resolveSecretRef(ctx context.Context, field, s string) (string, error) {
	if !isSecretRef(s) {
		return s, nil
	}
	ctx, cancel := context.WithTimeout(ctx, secretRefTimeout)
	defer cancel()
	value, err := secretResolver.Resolve(ctx, s)
	if err != nil {
		return "", fmt.Errorf("%s: resolve %s: %w", field, s, err)
	}
	return value, nil
}

// resolveEntrySecrets resolves secret references in every field that can carry
// a credential, mutating entry in place.
//
// A reference that fails to resolve is a hard error rather than a silent empty
// value: spawning an upstream with a blank key produces failures far from their
// cause. The operator ordering that follows from this is "populate the keychain
// entry, then switch the catalog to the reference" — never the reverse.
func resolveEntrySecrets(ctx context.Context, entry *MCPServerEntry) error {
	var err error
	if entry.Token, err = resolveSecretRef(ctx, "token", entry.Token); err != nil {
		return err
	}
	if entry.URL, err = resolveSecretRef(ctx, "url", entry.URL); err != nil {
		return err
	}
	if len(entry.Args) > 0 {
		args := make([]string, len(entry.Args))
		for i, arg := range entry.Args {
			if args[i], err = resolveSecretRef(ctx, fmt.Sprintf("args[%d]", i), arg); err != nil {
				return err
			}
		}
		entry.Args = args
	}
	if len(entry.Env) > 0 {
		env := make(map[string]string, len(entry.Env))
		for key, value := range entry.Env {
			if env[key], err = resolveSecretRef(ctx, "env."+key, value); err != nil {
				return err
			}
		}
		entry.Env = env
	}
	return nil
}

// LoadMCPServers reads all *.yaml files from <catalogDir>/mcp-servers/,
// parses them as MCPServerEntry, expands ${VAR} references, resolves secret
// references, and returns the enabled entries. A missing directory is silently
// treated as empty.
//
// This is the spawn-time loader: every caller feeds the result to a ClientPool.
// Resolution happens here rather than in LoadMCPServerCatalog so that secret
// material never reaches the catalog display and edit surfaces, which read the
// unresolved file instead.
func LoadMCPServers(catalogDir string) ([]MCPServerEntry, error) {
	entries, err := LoadMCPServerCatalog(catalogDir)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	out := entries[:0]
	for _, entry := range entries {
		if !entry.IsEnabled() {
			continue
		}
		if err := resolveEntrySecrets(ctx, &entry); err != nil {
			return nil, fmt.Errorf("mcp server %q: %w", entry.ID, err)
		}
		out = append(out, entry)
	}
	return out, nil
}

// LoadMCPServerCatalog reads all upstream MCP server catalog entries,
// including disabled entries. GUI/config surfaces use this so disabled
// servers remain visible and can be re-enabled.
//
// Secret references are deliberately left unresolved here — a catalog listing
// shows keychain://openai/work, not the key behind it.
func LoadMCPServerCatalog(catalogDir string) ([]MCPServerEntry, error) {
	dir := filepath.Join(catalogDir, "mcp-servers")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read mcp-servers dir: %w", err)
	}

	var out []MCPServerEntry
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		ext := strings.ToLower(filepath.Ext(name))
		if ext != ".yaml" && ext != ".yml" {
			continue
		}

		path := filepath.Join(dir, name)
		b, err := os.ReadFile(path) //nolint:gosec // G304: catalog-sourced path
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}

		var entry MCPServerEntry
		if err := yaml.Unmarshal(b, &entry); err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}

		// Expand ${VAR} references at load time.
		entry.URL = expandEnvRefs(entry.URL)
		entry.Token = expandEnvRefs(entry.Token)
		for i, arg := range entry.Args {
			entry.Args[i] = expandEnvRefs(arg)
		}
		expanded := make(map[string]string, len(entry.Env))
		for k, v := range entry.Env {
			expanded[k] = expandEnvRefs(v)
		}
		entry.Env = expanded

		out = append(out, entry)
	}
	return out, nil
}
