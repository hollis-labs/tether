package config

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/hollis-labs/tether/internal/credfile"
	"github.com/hollis-labs/tether/internal/llm/secrets"
	"gopkg.in/yaml.v3"
)

// MCPConfineRemoteEnv marks a planted Codex proxy whose local process tree is protected.
const MCPConfineRemoteEnv = "TETHER_MCP_CONFINE_REMOTE"

// MCPServerEntry describes one upstream MCP server in the catalog.
// Files live at <catalogDir>/mcp-servers/*.yaml.
//
// Credential-bearing fields (Args, Env, Token, URL) accept a secret reference
// in place of a literal value:
//
//   - keychain://<authority>/<path> or helper://<name>/<path>, resolved through
//     a helper program;
//   - file:///<absolute path> or file://~/<path under home>, read from a private
//     file (mode 0600, owned by the current user; see package credfile), so the
//     catalog YAML does not carry the secret.
//
// References are resolved only by LoadMCPServers, at spawn time; see
// resolveEntrySecrets. A catalog listing shows the reference, never the value.
type MCPServerEntry struct {
	// AllowUnconfinedRemote is an operator opt-in for HTTP/SSE upstreams that
	// cannot inherit a protected Codex proxy's local filesystem confinement.
	AllowUnconfinedRemote bool              `yaml:"allow_unconfined_remote"`
	ID                    string            `yaml:"id"`
	Transport             string            `yaml:"transport"` // "stdio" | "sse" | "http"
	Command               string            `yaml:"command"`   // stdio: binary path
	Args                  []string          `yaml:"args"`      // stdio: arguments; support ${VAR} and secret refs
	Env                   map[string]string `yaml:"env"`       // env vars; values support ${VAR} and secret refs
	URL                   string            `yaml:"url"`       // sse, http: endpoint URL
	Token                 string            `yaml:"token"`     // bearer token, ${VAR} ref, or secret ref
	// ProxyServiceTokenFile is an explicit daemon-only upstream service
	// credential path. It is never resolved from worker environment variables.
	ProxyServiceTokenFile string   `yaml:"proxy_service_token_file"`
	Scopes                []string `yaml:"scopes"`
	Enabled               *bool    `yaml:"enabled"` // nil → defaults to true
	Tags                  []string `yaml:"tags"`

	// argumentRedactionValues carries resolved argument and URL secret material
	// to the process owner without exposing it through YAML serialization.
	argumentRedactionValues []string

	// catalogDir is the catalog this entry was read from. A file:// credential
	// that is a symlink may resolve only into it or the operator's home.
	catalogDir string

	// fileRefs names the credential fields whose YAML value, as the operator
	// wrote it, is a file:// reference ("token", "url", "args[i]", "env.KEY").
	// It is recorded BEFORE ${VAR} expansion, so only a reference the catalog
	// author wrote counts: a value that merely becomes file://... through an
	// environment variable (which a launch's caller can set) stays a literal.
	fileRefs map[string]bool

	// secretRefs marks keychain/helper references as authored in the catalog,
	// before environment substitution can produce a reference-looking value.
	secretRefs map[string]bool
}

// IsEnabled returns true when the entry should be loaded. A missing enabled
// field (nil pointer) is treated as true.
func (e *MCPServerEntry) IsEnabled() bool {
	return e.Enabled == nil || *e.Enabled
}

// ArgumentRedactionValues returns secret material resolved from argument or url
// references and environment substitutions in them. The caller receives a copy.
func (e *MCPServerEntry) ArgumentRedactionValues() []string {
	return append([]string(nil), e.argumentRedactionValues...)
}

var envVarRE = regexp.MustCompile(`\$\{([^}]+)\}`)

// expandEnvRefs replaces ${VAR} tokens with os.Getenv(VAR) values.
func expandEnvRefs(s string) string {
	return envVarRE.ReplaceAllStringFunc(s, func(match string) string {
		key := match[2 : len(match)-1] // strip ${ and }
		return os.Getenv(key)
	})
}

func envRefValues(s string) []string {
	matches := envVarRE.FindAllStringSubmatch(s, -1)
	values := make([]string, 0, len(matches))
	for _, match := range matches {
		values = append(values, os.Getenv(match[1]))
	}
	return values
}

// secretRefTimeout bounds one helper invocation. Resolution shells out to
// tether-apikey-helper, which on macOS may block on a keychain ACL prompt.
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

// fileRefPrefix marks a credential stored in a private file.
const fileRefPrefix = "file://"

// isFileRef reports whether s names a credential file. It is separate from
// isSecretRef: the A2A config also resolves through resolveSecretRef, and a
// file:// there stays a literal.
func isFileRef(s string) bool {
	return strings.HasPrefix(s, fileRefPrefix)
}

// urlRedactionValues lists the strings under which a secret URL can appear in an
// error. net/http re-serializes the URL it fails on: the scheme is lower-cased,
// a path with characters such as ^ * ( ) [ ] or a space or non-ASCII is
// percent-encoded, and a userinfo password is masked as ***. So the URL as
// written is not enough: this also returns the parsed form, the path in both
// spellings, the query, each query value, and the userinfo password.
func urlRedactionValues(raw string) []string {
	values := []string{raw}
	u, err := url.Parse(raw)
	if err != nil {
		return values
	}
	values = append(values, u.String())
	if u.Path != "" && u.Path != "/" {
		values = append(values, u.Path, u.EscapedPath())
	}
	if u.RawQuery != "" {
		values = append(values, u.RawQuery)
		for _, vs := range u.Query() {
			values = append(values, vs...)
		}
	}
	if u.User != nil {
		if pw, ok := u.User.Password(); ok {
			values = append(values, pw)
		}
	}
	return values
}

// resolveFileRef reads the credential file s names. The path is whatever follows
// file://: absolute, or ~/ for the operator's home. It is used exactly as the
// catalog wrote it: ${VAR} is not expanded inside a file:// reference. (~ follows
// $HOME, which a launch's environment can set; an absolute path pins the file.)
//
// Like resolveSecretRef, the value never reaches the returned error: only the
// field and the path (the reference), which are non-sensitive by construction.
func resolveFileRef(field, s, catalogDir string) (string, error) {
	roots := []string{catalogDir}
	if home, err := os.UserHomeDir(); err == nil {
		roots = append(roots, home)
	}
	value, err := credfile.Read(strings.TrimPrefix(s, fileRefPrefix), credfile.Options{Roots: roots})
	if err != nil {
		return "", fmt.Errorf("%s: resolve %s: %w", field, s, err)
	}
	return value, nil
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
	resolve := func(field, s string) (string, error) {
		// Resolve only references the operator wrote in the YAML.
		if entry.fileRefs[field] && isFileRef(s) {
			return resolveFileRef(field, s, entry.catalogDir)
		}
		if entry.secretRefs[field] {
			return resolveSecretRef(ctx, field, s)
		}
		return s, nil
	}
	var err error
	if entry.Token, err = resolve("token", entry.Token); err != nil {
		return err
	}
	rawURL := entry.URL
	if entry.URL, err = resolve("url", entry.URL); err != nil {
		return err
	}
	if entry.URL != rawURL {
		// A url that came from a reference is itself a secret (for example
		// https://host/<token>). It reaches connect errors and logs, which scrub
		// these values, and net/http does not print it as written; see urlRedactionValues.
		entry.argumentRedactionValues = append(entry.argumentRedactionValues, urlRedactionValues(entry.URL)...)
	}
	if len(entry.Args) > 0 {
		args := make([]string, len(entry.Args))
		for i, arg := range entry.Args {
			field := fmt.Sprintf("args[%d]", i)
			secretRef := entry.secretRefs[field] || entry.fileRefs[field]
			if args[i], err = resolve(field, arg); err != nil {
				return err
			}
			if secretRef {
				entry.argumentRedactionValues = append(entry.argumentRedactionValues, args[i])
			}
		}
		entry.Args = args
	}
	if len(entry.Env) > 0 {
		env := make(map[string]string, len(entry.Env))
		for key, value := range entry.Env {
			if env[key], err = resolve("env."+key, value); err != nil {
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

// LoadMCPServersConfined is LoadMCPServers for a proxy confined to ids
// (CW-20261001-0227): only the enabled entries named in ids are returned, and
// only they have their secret references resolved. An upstream outside the
// set contributes nothing -- its credentials never reach the proxy's memory,
// and a reference of its that fails to resolve cannot stop the proxy.
//
// unknown lists the ids that name no enabled entry (a typo, a disabled or a
// removed server), in the order given, so the caller can say so; they are
// otherwise ignored, which narrows the confined set rather than widening it.
func LoadMCPServersConfined(catalogDir string, ids []string) (entries []MCPServerEntry, unknown []string, err error) {
	catalog, err := LoadMCPServerCatalog(catalogDir)
	if err != nil {
		return nil, nil, err
	}
	want := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		want[id] = struct{}{}
	}
	ctx := context.Background()
	found := make(map[string]struct{}, len(ids))
	for _, entry := range catalog {
		if _, ok := want[entry.ID]; !ok || !entry.IsEnabled() {
			continue
		}
		if err := resolveEntrySecrets(ctx, &entry); err != nil {
			return nil, nil, fmt.Errorf("mcp server %q: %w", entry.ID, err)
		}
		entries = append(entries, entry)
		found[entry.ID] = struct{}{}
	}
	seen := map[string]struct{}{}
	for _, id := range ids {
		if _, ok := found[id]; ok {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		unknown = append(unknown, id)
	}
	return entries, unknown, nil
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

		entry.catalogDir = catalogDir
		entry.fileRefs = map[string]bool{}
		entry.secretRefs = map[string]bool{}

		// Expand ${VAR} references at load time, except inside a file://
		// reference. Record every reference scheme before expansion: a value
		// that only becomes file://, keychain:// or helper:// through an
		// environment variable stays a literal.
		expand := func(field, raw string) string {
			if isFileRef(raw) {
				entry.fileRefs[field] = true
				return raw
			}
			if isSecretRef(raw) {
				entry.secretRefs[field] = true
			}
			return expandEnvRefs(raw)
		}
		if !isFileRef(entry.URL) {
			entry.argumentRedactionValues = append(entry.argumentRedactionValues, envRefValues(entry.URL)...)
		}
		entry.URL = expand("url", entry.URL)
		entry.Token = expand("token", entry.Token)
		for i, arg := range entry.Args {
			field := fmt.Sprintf("args[%d]", i)
			if !isFileRef(arg) {
				entry.argumentRedactionValues = append(entry.argumentRedactionValues, envRefValues(arg)...)
			}
			entry.Args[i] = expand(field, arg)
		}
		expanded := make(map[string]string, len(entry.Env))
		for k, v := range entry.Env {
			expanded[k] = expand("env."+k, v)
		}
		entry.Env = expanded

		out = append(out, entry)
	}
	return out, nil
}
