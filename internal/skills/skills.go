// Package skills loads agent skills authored as markdown files with YAML
// frontmatter and compiles them into per-provider planted-file shapes for
// the BootDirSpec pipeline (v005-08 Agent Ops).
//
// A skill file has the shape:
//
//	---
//	id: refactor-go
//	name: Refactor Go
//	description: Apply Go refactoring patterns
//	triggers: [refactor, cleanup]
//	---
//	<skill body in Markdown>
//
// Provider conventions (initial in-scope set):
//
//	Claude → .claude/skills/<id>.md (one file per skill)
//	Codex  → AGENTS.md inline sections (all skills aggregated into one file)
//
// Other providers (Opencode, Nanite-headless) are out-of-scope for v005-08
// and return ErrUnsupportedProvider. Compilers are intentionally additive —
// adding a new provider compiler is a contained change; resist designing a
// generic skill IR until a third compiler proves the pattern.
package skills

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ErrUnsupportedProvider is returned by CompileForProvider when the providerID
// has no registered compiler for v005-08 scope.
var ErrUnsupportedProvider = errors.New("skills: unsupported provider for compilation")

// Skill is a parsed skill file. Fields mirror the frontmatter schema plus the
// markdown body trimmed of leading/trailing whitespace.
type Skill struct {
	ID          string   `yaml:"id"`
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Triggers    []string `yaml:"triggers"`
	// Body is the markdown content after the closing frontmatter delimiter.
	// Populated by Parse; not declared in the YAML schema.
	Body string `yaml:"-"`
	// Path is the absolute path the skill was loaded from, when known.
	// Useful for `mux agents show` diagnostics. Empty for inline parses.
	Path string `yaml:"-"`
}

// CompiledFile is the provider-agnostic output of a skill compiler. The
// integration layer (service.go BootDirSpec compilation) wraps this into
// provider.PlantedFile shape — keeping CompiledFile here avoids a hard
// dependency on go-providers from the skills package.
type CompiledFile struct {
	// RelPath is the file path relative to the BootDir root.
	RelPath string
	// Content is the file body.
	Content string
	// Mode is the file mode; zero means the integration layer's default (0o644).
	Mode os.FileMode
}

// Parse parses a single skill file's bytes. Returns an error if the
// frontmatter delimiters are missing, malformed, or the parsed `id` is empty.
func Parse(data []byte) (Skill, error) {
	front, body, err := splitFrontmatter(data)
	if err != nil {
		return Skill{}, err
	}
	var s Skill
	if err := yaml.Unmarshal(front, &s); err != nil {
		return Skill{}, fmt.Errorf("parse frontmatter: %w", err)
	}
	if s.ID == "" {
		return Skill{}, fmt.Errorf("skill: missing required field id")
	}
	s.Body = strings.TrimSpace(string(body))
	return s, nil
}

// ParseFile reads a skill file from disk, parses it, and stamps the absolute
// path on the returned Skill.
func ParseFile(path string) (Skill, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: catalog-sourced path
	if err != nil {
		return Skill{}, fmt.Errorf("read %s: %w", path, err)
	}
	s, err := Parse(data)
	if err != nil {
		return Skill{}, fmt.Errorf("parse %s: %w", path, err)
	}
	s.Path = path
	return s, nil
}

// LoadDir walks a directory of *.md skill files and returns them keyed by ID.
// Missing directory returns an empty map (not an error). Files that fail to
// parse abort the load — better to fail loud than silently skip.
func LoadDir(dir string) (map[string]Skill, error) {
	out := map[string]Skill{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return out, nil
		}
		return nil, fmt.Errorf("read skills dir %s: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".md" {
			continue
		}
		s, err := ParseFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		out[s.ID] = s
	}
	return out, nil
}

// splitFrontmatter extracts the YAML frontmatter block (between leading `---`
// and the next `---`) and the remaining body. The leading delimiter must be
// the first non-empty content of the file; surrounding whitespace is tolerated.
func splitFrontmatter(data []byte) (front, body []byte, err error) {
	s := string(data)
	trimmed := strings.TrimLeft(s, " \t\r\n")
	if !strings.HasPrefix(trimmed, "---") {
		return nil, nil, fmt.Errorf("missing leading --- frontmatter delimiter")
	}
	// Move into the body after the opening "---" line.
	after := strings.TrimPrefix(trimmed, "---")
	// after must start with a newline (else "---foo" is a header, not delim).
	if !strings.HasPrefix(after, "\n") && !strings.HasPrefix(after, "\r\n") {
		return nil, nil, fmt.Errorf("opening --- delimiter must be on its own line")
	}
	after = strings.TrimLeft(after, "\r\n")
	closeIdx := strings.Index(after, "\n---")
	if closeIdx < 0 {
		// allow EOF-terminated frontmatter (no body)
		return []byte(after), nil, nil
	}
	front = []byte(after[:closeIdx])
	rest := after[closeIdx+len("\n---"):]
	rest = strings.TrimLeft(rest, "\r\n")
	body = []byte(rest)
	return front, body, nil
}

// ─── Provider compilers ──────────────────────────────────────────────────────

// CompileForProvider dispatches to the per-provider compiler for the given
// providerID. ProviderIDs are matched case-insensitively against a small
// alias table — catalog Provider.ID, go-providers adapter name, and common
// shorthand all route to the same compiler.
func CompileForProvider(providerID string, allSkills []Skill) ([]CompiledFile, error) {
	switch normalizeProviderID(providerID) {
	case "claude":
		return CompileClaude(allSkills), nil
	case "codex":
		return CompileCodex(allSkills), nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedProvider, providerID)
	}
}

func normalizeProviderID(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	switch id {
	case "claude", "claude-code", "claude-stream", "claudecode", "claudestream":
		return "claude"
	case "codex", "codex-app-server", "codex-cli", "codexappserver", "codexcli":
		return "codex"
	default:
		return id
	}
}

// CompileClaude renders one .claude/skills/<id>.md file per skill. Skill body
// is preserved verbatim; the frontmatter is NOT re-emitted (Claude's skill
// loader expects markdown content). Skills are emitted in ID-sorted order
// for deterministic output.
func CompileClaude(allSkills []Skill) []CompiledFile {
	sorted := sortByID(allSkills)
	out := make([]CompiledFile, 0, len(sorted))
	for _, s := range sorted {
		out = append(out, CompiledFile{
			RelPath: filepath.Join(".claude", "skills", s.ID+".md"),
			Content: claudeSkillContent(s),
		})
	}
	return out
}

func claudeSkillContent(s Skill) string {
	var b strings.Builder
	if s.Name != "" {
		fmt.Fprintf(&b, "# %s\n\n", s.Name)
	}
	if s.Description != "" {
		fmt.Fprintf(&b, "%s\n\n", s.Description)
	}
	if len(s.Triggers) > 0 {
		fmt.Fprintf(&b, "**Triggers:** %s\n\n", strings.Join(s.Triggers, ", "))
	}
	b.WriteString(s.Body)
	if !strings.HasSuffix(s.Body, "\n") {
		b.WriteString("\n")
	}
	return b.String()
}

// CompileCodex aggregates every skill into a single AGENTS.md file with a
// `## Skill: <name>` section per entry. Sections are sorted by skill ID so
// output is byte-stable across runs. Returns a one-element slice; callers
// can append additional planted files alongside.
func CompileCodex(allSkills []Skill) []CompiledFile {
	if len(allSkills) == 0 {
		return nil
	}
	sorted := sortByID(allSkills)
	var b strings.Builder
	b.WriteString("# Skills\n\n")
	b.WriteString("This document lists the skills available to the agent. Each section is a self-contained guideline.\n\n")
	for _, s := range sorted {
		title := s.Name
		if title == "" {
			title = s.ID
		}
		fmt.Fprintf(&b, "## Skill: %s (`%s`)\n\n", title, s.ID)
		if s.Description != "" {
			fmt.Fprintf(&b, "%s\n\n", s.Description)
		}
		if len(s.Triggers) > 0 {
			fmt.Fprintf(&b, "**Triggers:** %s\n\n", strings.Join(s.Triggers, ", "))
		}
		b.WriteString(s.Body)
		b.WriteString("\n\n")
	}
	return []CompiledFile{{RelPath: "AGENTS.md", Content: strings.TrimRight(b.String(), "\n") + "\n"}}
}

func sortByID(in []Skill) []Skill {
	out := make([]Skill, len(in))
	copy(out, in)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// WriteSkillFile is a small convenience for tests and `mux agents create`
// scaffolding. It writes the canonical frontmatter+body shape so authored
// files round-trip through Parse.
func WriteSkillFile(w io.Writer, s Skill) error {
	front, err := yaml.Marshal(struct {
		ID          string   `yaml:"id"`
		Name        string   `yaml:"name"`
		Description string   `yaml:"description,omitempty"`
		Triggers    []string `yaml:"triggers,omitempty"`
	}{ID: s.ID, Name: s.Name, Description: s.Description, Triggers: s.Triggers})
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "---\n%s---\n\n%s", front, s.Body); err != nil {
		return err
	}
	if !strings.HasSuffix(s.Body, "\n") {
		if _, err := w.Write([]byte("\n")); err != nil {
			return err
		}
	}
	return nil
}
