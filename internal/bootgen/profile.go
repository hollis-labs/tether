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
//	static       — read one or more files from disk (supports glob + directories)
//	role_summary — summarize a role file and point to its full path
//	skill_index  — list skill pointers from layered discovery
//	cmd          — run a shell command and capture stdout
//	http         — GET a URL and use the response body
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
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
	"time"

	"github.com/hollis-labs/tether/internal/skills"
	"gopkg.in/yaml.v3"
)

// defaultHTTPClient is used for http slot sources.
var defaultHTTPClient = &http.Client{Timeout: 15 * time.Second}

// maxHTTPSlotBytes caps the bytes read from an http slot response so a
// misconfigured or malicious target URL can't exhaust memory via an
// unbounded body.
const maxHTTPSlotBytes int64 = 4 * 1024 * 1024

const defaultBootHTTPBaseURL = "http://127.0.0.1:8089"

// Profile is a boot profile configuration loaded from
// <catalog-root>/boot-profiles/<id>.yaml.
type Profile struct {
	ID          string   `yaml:"id"`
	DisplayName string   `yaml:"display_name"`
	Tags        []string `yaml:"tags,omitempty"`
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
	// Type is "static", "role_summary", "skill_index", "cmd", or "http".
	Type string `yaml:"type"`

	// Static source fields.
	// Path may be a file or directory. ~ is expanded.
	Path string `yaml:"path,omitempty"`
	// Glob is applied inside a directory Path. Default: "*.md"
	Glob string `yaml:"glob,omitempty"`
	// Limit caps the number of files when Glob produces multiple matches.
	Limit int `yaml:"limit,omitempty"`
	// PreferTriggers nudges skill_index ordering toward the listed trigger terms.
	PreferTriggers []string `yaml:"prefer_triggers,omitempty"`

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
		content, err := resolveSlot(ctx, p, src, catalogRoot)
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

func resolveSlot(ctx context.Context, p Profile, src SlotSource, catalogRoot string) (string, error) {
	switch src.Type {
	case "static":
		return resolveStatic(src, catalogRoot)
	case "role_summary":
		return resolveRoleSummary(src, catalogRoot)
	case "skill_index":
		return resolveSkillIndex(p, src, catalogRoot)
	case "cmd":
		return resolveCmd(ctx, src)
	case "http":
		return resolveHTTP(ctx, src)
	case "":
		return "", fmt.Errorf("slot source missing type")
	default:
		return "", fmt.Errorf("unknown source type %q (supported: static, role_summary, skill_index, cmd, http)", src.Type)
	}
}

func resolveSkillIndex(p Profile, src SlotSource, catalogRoot string) (string, error) {
	workingDir, _ := os.Getwd()
	all, err := skills.DiscoverLayered(catalogRoot, workingDir)
	if err != nil {
		return "", err
	}
	rankSkillsForProfile(all, p, src.PreferTriggers)
	limit := src.Limit
	if limit <= 0 || limit > len(all) {
		limit = len(all)
	}
	var lines []string
	for _, s := range all[:limit] {
		description := strings.TrimSpace(s.Skill.Description)
		if description == "" {
			description = strings.TrimSpace(s.Skill.Name)
		}
		if description == "" {
			description = "No description"
		}
		lines = append(lines, fmt.Sprintf("/%s — %s", s.Skill.ID, description))
	}
	return strings.Join(lines, "\n"), nil
}

func rankSkillsForProfile(all []skills.LayeredSkill, p Profile, preferTriggers []string) {
	profileSignals := makeProfileSignalSet(p)
	fallbackSignals := makeFallbackSignalSet(p)
	preferredWeights := makeOrderedWeightSet(preferTriggers)
	sort.SliceStable(all, func(i, j int) bool {
		left := scoreSkill(all[i].Skill, profileSignals, fallbackSignals, preferredWeights)
		right := scoreSkill(all[j].Skill, profileSignals, fallbackSignals, preferredWeights)
		switch {
		case left.coreMatches != right.coreMatches:
			return left.coreMatches > right.coreMatches
		case left.preferredMatches != right.preferredMatches:
			return left.preferredMatches > right.preferredMatches
		case left.priority != right.priority:
			return left.priority > right.priority
		default:
			return all[i].Skill.ID < all[j].Skill.ID
		}
	})
}

type skillScore struct {
	coreMatches      int
	preferredMatches int
	priority         int
}

func scoreSkill(s skills.Skill, profileSignals, fallbackSignals map[string]struct{}, preferredWeights map[string]int) skillScore {
	score := skillScore{priority: s.Priority}
	signals := skillSignals(s)
	matchingSignals := profileSignals
	if len(s.Triggers) == 0 {
		matchingSignals = fallbackSignals
	}
	for _, signal := range signals {
		if _, ok := matchingSignals[signal]; ok {
			score.coreMatches++
		}
		if weight, ok := preferredWeights[signal]; ok {
			score.preferredMatches += weight
		}
	}
	return score
}

func skillSignals(s skills.Skill) []string {
	if len(s.Triggers) > 0 {
		return normalizeStringList(s.Triggers)
	}
	return normalizeStringList(strings.FieldsFunc(
		strings.Join([]string{s.ID, s.Name, s.Description}, " "),
		func(r rune) bool {
			return (r < 'a' || r > 'z') &&
				(r < 'A' || r > 'Z') &&
				(r < '0' || r > '9')
		},
	))
}

func makeProfileSignalSet(p Profile) map[string]struct{} {
	var signals []string
	signals = append(signals, p.Identity.Role, p.Identity.Project)
	signals = append(signals, p.Tags...)
	return normalizeStringSet(signals)
}

func makeFallbackSignalSet(p Profile) map[string]struct{} {
	var signals []string
	signals = append(signals, p.Identity.Role)
	signals = append(signals, p.Tags...)
	return normalizeStringSet(signals)
}

func normalizeStringSet(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range normalizeStringList(values) {
		out[value] = struct{}{}
	}
	return out
}

func normalizeStringList(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func makeOrderedWeightSet(values []string) map[string]int {
	normalized := normalizeStringList(values)
	out := make(map[string]int, len(normalized))
	for i, value := range normalized {
		out[value] = len(normalized) - i
	}
	return out
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

func resolveRoleSummary(src SlotSource, catalogRoot string) (string, error) {
	path := resolvePath(src.Path, catalogRoot)
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", path, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("role_summary path must be a file: %s", path)
	}
	b, err := os.ReadFile(path) //nolint:gosec
	if err != nil {
		return "", err
	}

	title, mission := summarizeRoleMarkdown(string(b), path)
	var sb strings.Builder
	fmt.Fprintf(&sb, "### Role Identity\n\n")
	fmt.Fprintf(&sb, "- Role: %s\n", title)
	fmt.Fprintf(&sb, "- Full role file: `%s`\n\n", path)
	fmt.Fprintf(&sb, "%s", mission)
	return sb.String(), nil
}

func summarizeRoleMarkdown(markdown, path string) (string, string) {
	lines := stripYAMLFrontMatter(strings.Split(markdown, "\n"))
	title := firstMarkdownHeading(lines)
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	title = strings.TrimSpace(strings.TrimPrefix(title, "Role:"))
	mission := paragraphAfterHeading(lines, "mission", "purpose")
	if mission == "" {
		mission = firstMarkdownParagraph(lines)
	}
	if mission == "" {
		mission = "See the full role file for mission details."
	}
	return title, mission
}

func stripYAMLFrontMatter(lines []string) []string {
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return lines
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return lines[i+1:]
		}
	}
	return lines
}

func firstMarkdownHeading(lines []string) string {
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			return strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
		}
	}
	return ""
}

func paragraphAfterHeading(lines []string, headingWords ...string) string {
	for i, line := range lines {
		heading := strings.TrimSpace(line)
		if !strings.HasPrefix(heading, "#") {
			continue
		}
		heading = strings.ToLower(strings.TrimSpace(strings.TrimLeft(heading, "#")))
		for _, word := range headingWords {
			if strings.Contains(heading, word) {
				return paragraphFrom(lines[i+1:])
			}
		}
	}
	return ""
}

func firstMarkdownParagraph(lines []string) string {
	return paragraphFrom(lines)
}

func paragraphFrom(lines []string) string {
	var parts []string
	inCodeBlock := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inCodeBlock = !inCodeBlock
			continue
		}
		if inCodeBlock {
			continue
		}
		if trimmed == "" {
			if len(parts) > 0 {
				break
			}
			continue
		}
		if isStructuralMarkdownLine(trimmed) {
			if len(parts) > 0 {
				break
			}
			continue
		}
		parts = append(parts, trimmed)
	}
	return strings.Join(parts, " ")
}

func isStructuralMarkdownLine(line string) bool {
	return strings.HasPrefix(line, "#") ||
		strings.HasPrefix(line, "- ") ||
		strings.HasPrefix(line, "* ") ||
		strings.HasPrefix(line, ">") ||
		strings.HasPrefix(line, "|") ||
		strings.HasPrefix(line, "```")
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
	rawURL, err := resolveHTTPURL(src)
	if err != nil {
		return "", err
	}
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

func resolveHTTPURL(src SlotSource) (string, error) {
	// Expand env vars in URL so profiles can use ${TESSERACT_URL}, ${VANTA_URL}, etc.
	rawURL := os.ExpandEnv(src.URL)
	if rawURL == "" {
		return "", nil
	}
	if !strings.HasPrefix(rawURL, "/") {
		return rawURL, nil
	}

	baseURL := bootHTTPBaseURL()
	base, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("parse boot HTTP base URL %q: %w", baseURL, err)
	}
	if base.Scheme == "" || base.Host == "" {
		return "", fmt.Errorf("boot HTTP base URL %q must include scheme and host", baseURL)
	}
	rel, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse relative http slot URL %q: %w", rawURL, err)
	}
	return base.ResolveReference(rel).String(), nil
}

func bootHTTPBaseURL() string {
	for _, key := range []string{"TETHER_BOOT_HTTP_BASE_URL", "TESSERACT_URL", "VANTA_URL"} {
		if value := strings.TrimRight(os.Getenv(key), "/"); value != "" {
			return value
		}
	}
	return defaultBootHTTPBaseURL
}

func expandPath(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[2:])
		}
	}
	return os.ExpandEnv(p)
}

func resolvePath(path, catalogRoot string) string {
	path = expandPath(path)
	if !filepath.IsAbs(path) {
		path = filepath.Join(catalogRoot, path)
	}
	return path
}
