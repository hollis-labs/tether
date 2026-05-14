// Package bootgen implements the boot prompt generator for mux generate-boot.
//
// A boot profile is a YAML file under <catalog-root>/boot-profiles/ that
// declares how to populate each named slot. Slots are assembled into the
// canonical 7-section shape (or a custom template) and written to stdout
// so callers can pipe directly to their CLI tool:
//
//	mux generate-boot nanite.backend.main | claude --dangerously-skip-permissions
//
// Slot source types:
//
//	static — read one or more files from disk (supports glob + directories)
//	cmd    — run a shell command and capture stdout
//	http   — GET a URL and use the response body
package bootgen

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"gopkg.in/yaml.v3"
)

// defaultHTTPClient is used for http slot sources.
var defaultHTTPClient = &http.Client{Timeout: 15 * time.Second}

// maxHTTPSlotBytes caps the bytes read from an http slot response so a
// misconfigured or malicious target URL can't exhaust memory via an
// unbounded body.
const maxHTTPSlotBytes int64 = 4 * 1024 * 1024

// Profile is a boot profile configuration loaded from
// <catalog-root>/boot-profiles/<id>.yaml.
type Profile struct {
	ID          string `yaml:"id"`
	DisplayName string `yaml:"display_name"`
	// Launch is the catalog launch ID to use when this profile is used to
	// start a session via `mux boot <profile_id>` or the TUI boot-launch flow.
	// When empty, generate-boot only writes to stdout (no session created).
	Launch string `yaml:"launch,omitempty"`
	// Identity carries the agent identity fields (Agent Identity Model,
	// chatgpt-research-01-2026-04-21.md). Populates §1 of the boot prompt.
	Identity Identity              `yaml:"identity"`
	Slots    map[string]SlotSource `yaml:"slots"`
	// Template is optional; when empty the default 7-section template is used.
	Template string `yaml:"template,omitempty"`
	// MCPServers is the allowlist of upstream MCP server IDs this boot profile
	// exposes via the proxy at launch time (v005-08). Pipes through to
	// `mux mcp --proxy --servers <ids>`. Empty = no allowlist (proxy default).
	// The list lives on the boot profile, not the agent, so a single agent
	// can have multiple profiles with different tool surfaces.
	MCPServers []string `yaml:"mcp_servers,omitempty"`
}

// Identity holds agent identity metadata per the Agent Identity Model:
//   - lineage_alias  = <project>.<role>.<profile> dot notation (scope_key)
//   - lineage_id     = stable machine ID (format: agtln_<ulid>; issued by Agent Mux)
//   - profile_id     = config name (e.g. "nanite-backend")
//   - profile_version = integer revision counter for this profile config
//   - role / project / work_root / tracking_root — contextual metadata
type Identity struct {
	LineageAlias   string `yaml:"lineage_alias"`
	LineageID      string `yaml:"lineage_id,omitempty"`
	ProfileID      string `yaml:"profile_id,omitempty"`
	ProfileVersion int    `yaml:"profile_version,omitempty"`
	Role           string `yaml:"role,omitempty"`
	Project        string `yaml:"project,omitempty"`
	WorkRoot       string `yaml:"work_root,omitempty"`
	TrackingRoot   string `yaml:"tracking_root,omitempty"`
	VantaPrimary   string `yaml:"vanta_primary,omitempty"`
}

// SlotSource describes how to populate a single named slot.
type SlotSource struct {
	// Type is "static", "cmd", or "http".
	Type string `yaml:"type"`

	// Static source fields.
	// Path may be a file or directory. ~ is expanded.
	Path string `yaml:"path,omitempty"`
	// Glob is applied inside a directory Path. Default: "*.md"
	Glob string `yaml:"glob,omitempty"`
	// Limit caps the number of files when Glob produces multiple matches.
	Limit int `yaml:"limit,omitempty"`

	// Cmd source fields.
	Run     string `yaml:"run,omitempty"`
	Timeout string `yaml:"timeout,omitempty"`

	// HTTP source fields.
	URL string `yaml:"url,omitempty"`
	// ResponseFormat controls how the HTTP response body is rendered.
	// "text" (default): use body verbatim.
	// "vanta_recall": parse Vanta GET /v1/recall JSON and render as markdown bullet list.
	ResponseFormat string `yaml:"response_format,omitempty"`
}

// LoadProfile reads and parses a single boot profile YAML.
func LoadProfile(path string) (Profile, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: catalog-sourced path
	if err != nil {
		return Profile{}, fmt.Errorf("read boot profile %s: %w", path, err)
	}
	var p Profile
	if err := yaml.Unmarshal(b, &p); err != nil {
		return Profile{}, fmt.Errorf("parse boot profile %s: %w", path, err)
	}
	if p.ID == "" {
		return Profile{}, fmt.Errorf("boot profile %s: missing id field", path)
	}
	return p, nil
}

// LoadProfiles reads all *.yaml files in dir and returns them keyed by ID.
// Missing directory returns empty map (not an error).
func LoadProfiles(dir string) (map[string]Profile, error) {
	out := map[string]Profile{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return out, nil
		}
		return nil, fmt.Errorf("read boot-profiles dir %s: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		p, err := LoadProfile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		out[p.ID] = p
	}
	return out, nil
}

// Generate assembles the boot prompt for the given profile and writes it to w.
// Slot resolution failures are surfaced as inline comments so the session can
// still start with reduced context.
func Generate(ctx context.Context, p Profile, catalogRoot string, w io.Writer) error {
	resolved := make(map[string]string, len(p.Slots))
	for name, src := range p.Slots {
		content, err := resolveSlot(ctx, src, catalogRoot)
		if err != nil {
			resolved[name] = fmt.Sprintf("<!-- slot:%s resolution failed: %v -->", name, err)
		} else {
			resolved[name] = content
		}
	}

	var tmplText string
	if p.Template != "" {
		b, err := os.ReadFile(expandPath(p.Template)) //nolint:gosec
		if err != nil {
			return fmt.Errorf("read boot template %s: %w", p.Template, err)
		}
		tmplText = string(b)
	} else {
		tmplText = defaultTemplate
	}

	tmpl, err := template.New("boot").Funcs(template.FuncMap{
		"slot": func(name string) string {
			return resolved[name]
		},
		"hasSlot": func(name string) bool {
			v, ok := resolved[name]
			return ok && strings.TrimSpace(v) != ""
		},
		"now": func() string { return time.Now().UTC().Format(time.RFC3339) },
	}).Parse(tmplText)
	if err != nil {
		return fmt.Errorf("parse boot template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]any{
		"Profile":    p,
		"Slots":      resolved,
		"CompiledAt": time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		return fmt.Errorf("render boot prompt: %w", err)
	}
	_, err = io.Copy(w, &buf)
	return err
}

// ─── slot resolution ─────────────────────────────────────────────────────────

func resolveSlot(ctx context.Context, src SlotSource, catalogRoot string) (string, error) {
	switch src.Type {
	case "static":
		return resolveStatic(src, catalogRoot)
	case "cmd":
		return resolveCmd(ctx, src)
	case "http":
		return resolveHTTP(ctx, src)
	case "":
		return "", fmt.Errorf("slot source missing type")
	default:
		return "", fmt.Errorf("unknown source type %q (supported: static, cmd, http)", src.Type)
	}
}

func resolveStatic(src SlotSource, catalogRoot string) (string, error) {
	path := expandPath(src.Path)
	if !filepath.IsAbs(path) {
		path = filepath.Join(catalogRoot, path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", path, err)
	}
	if !info.IsDir() {
		b, err := os.ReadFile(path) //nolint:gosec
		return string(b), err
	}
	glob := src.Glob
	if glob == "" {
		glob = "*.md"
	}
	matches, err := filepath.Glob(filepath.Join(path, glob))
	if err != nil {
		return "", fmt.Errorf("glob: %w", err)
	}
	limit := src.Limit
	if limit <= 0 || limit > len(matches) {
		limit = len(matches)
	}
	var parts []string
	for _, m := range matches[:limit] {
		b, readErr := os.ReadFile(m) //nolint:gosec
		if readErr != nil {
			continue
		}
		parts = append(parts, fmt.Sprintf("### %s\n\n%s", filepath.Base(m), string(b)))
	}
	return strings.Join(parts, "\n\n---\n\n"), nil
}

func resolveCmd(ctx context.Context, src SlotSource) (string, error) {
	timeout := 10 * time.Second
	if src.Timeout != "" {
		d, err := time.ParseDuration(src.Timeout)
		if err != nil {
			return "", fmt.Errorf("invalid timeout %q: %w", src.Timeout, err)
		}
		timeout = d
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", src.Run) //nolint:gosec // G204: user-authored catalog command
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", fmt.Errorf("command %q exited %d: %s", src.Run, exitErr.ExitCode(), string(exitErr.Stderr))
		}
		return "", fmt.Errorf("command %q: %w", src.Run, err)
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// vantaRecallBrief is the shape of a single item in a Vanta GET /v1/recall
// response when format=brief. Only the fields we render are declared.
type vantaRecallBrief struct {
	MemoryKey  string   `json:"memory_key"`
	Domain     string   `json:"domain"`
	Tags       []string `json:"tags"`
	Confidence float64  `json:"confidence"`
	Summary    string   `json:"summary"`
	CreatedAt  string   `json:"created_at"`
}

type vantaRecallResponse struct {
	Results []vantaRecallBrief `json:"results"`
	Meta    struct {
		Namespace string `json:"namespace"`
		Returned  int    `json:"returned"`
	} `json:"meta"`
}

// formatVantaRecall converts a Vanta GET /v1/recall JSON response into a
// readable markdown bullet list for insertion into boot prompt slots.
func formatVantaRecall(body string) string {
	var resp vantaRecallResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return body // not parseable — return raw
	}
	if resp.Meta.Returned == 0 {
		return "" // empty result = omit the section
	}
	var sb strings.Builder
	for _, item := range resp.Results {
		key := item.MemoryKey
		if key == "" {
			key = item.Domain
		}
		fmt.Fprintf(&sb, "- **%s** — %s\n", key, item.Summary)
	}
	return strings.TrimRight(sb.String(), "\n")
}

func resolveHTTP(ctx context.Context, src SlotSource) (string, error) {
	// Expand env vars in URL so profiles can use ${VANTA_URL}, ${CLOCKWORK_URL}, etc.
	rawURL := os.ExpandEnv(src.URL)
	if rawURL == "" {
		return "", fmt.Errorf("http slot URL is empty after env expansion")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", fmt.Errorf("build request %s: %w", rawURL, err)
	}
	// Optional auth header via VANTA_TOKEN / bearer token env var.
	if token := os.Getenv("VANTA_TOKEN"); token != "" && strings.Contains(rawURL, "vanta") {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := defaultHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("GET %s: status %d", rawURL, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPSlotBytes+1))
	if err != nil {
		return "", err
	}
	if int64(len(b)) > maxHTTPSlotBytes {
		return "", fmt.Errorf("GET %s: response exceeds %d-byte slot cap", rawURL, maxHTTPSlotBytes)
	}
	body := string(b)
	switch strings.ToLower(src.ResponseFormat) {
	case "vanta_recall", "tesseract_recall":
		return formatVantaRecall(body), nil
	}
	return body, nil
}

func expandPath(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[2:])
		}
	}
	return os.ExpandEnv(p)
}
