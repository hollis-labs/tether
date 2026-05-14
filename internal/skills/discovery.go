package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hollis-labs/tether/internal/config"
)

// LayeredSkill pairs a parsed Skill with the discovery layer that supplied it.
type LayeredSkill struct {
	Skill Skill
	Layer string
}

// DiscoverLayered returns every skill visible through Tether's layered
// discovery stack plus legacy user skill directories. Earlier entries in the
// resolver chain win; fallback directories only fill IDs not already present.
func DiscoverLayered(catalogRoot, workingDir string) ([]LayeredSkill, error) {
	cat, err := config.Discover(config.DefaultLayers(catalogRoot, workingDir))
	if err != nil {
		return nil, fmt.Errorf("discover skills: %w", err)
	}

	byID := map[string]LayeredSkill{}
	for id, lp := range cat.SkillPaths {
		s, err := ParseFile(lp.Path)
		if err != nil {
			return nil, fmt.Errorf("skill %q: %w", id, err)
		}
		byID[id] = LayeredSkill{Skill: s, Layer: lp.Layer.String()}
	}

	for _, dir := range LegacySkillDirs() {
		loaded, err := loadLegacyDir(dir.Path)
		if err != nil {
			return nil, err
		}
		for id, s := range loaded {
			if _, exists := byID[id]; exists {
				continue
			}
			byID[id] = LayeredSkill{Skill: s, Layer: dir.Layer}
		}
	}

	out := make([]LayeredSkill, 0, len(byID))
	for _, s := range byID {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Skill.ID < out[j].Skill.ID
	})
	return out, nil
}

// ResolveLayered returns one skill by ID from the same chain as
// DiscoverLayered.
func ResolveLayered(catalogRoot, workingDir, id string) (LayeredSkill, error) {
	cat, err := config.Discover(config.DefaultLayers(catalogRoot, workingDir))
	if err != nil {
		return LayeredSkill{}, fmt.Errorf("discover skills: %w", err)
	}

	if lp, ok := cat.SkillPaths[id]; ok {
		s, err := ParseFile(lp.Path)
		if err != nil {
			return LayeredSkill{}, fmt.Errorf("skill %q: %w", id, err)
		}
		return LayeredSkill{Skill: s, Layer: lp.Layer.String()}, nil
	}

	for _, dir := range LegacySkillDirs() {
		path := filepath.Join(dir.Path, id+".md")
		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return LayeredSkill{}, fmt.Errorf("stat skill %s: %w", path, err)
		}
		s, err := parseLegacySkillFile(path)
		if err != nil {
			return LayeredSkill{}, fmt.Errorf("skill %q: %w", id, err)
		}
		return LayeredSkill{Skill: s, Layer: dir.Layer}, nil
	}
	return LayeredSkill{}, fmt.Errorf("skill %q not found in catalog or legacy skill directories", id)
}

// LegacySkillDir is a user-level skill directory outside the layered catalog.
type LegacySkillDir struct {
	Layer string
	Path  string
}

// LegacySkillDirs returns post-rename and legacy skill directories. Missing
// directories are handled by LoadDir.
func LegacySkillDirs() []LegacySkillDir {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []LegacySkillDir{
		{Layer: "user-tether", Path: filepath.Join(home, ".tether", "skills")},
		{Layer: "legacy-nanite", Path: filepath.Join(home, ".nanite", "skills")},
	}
}

func loadLegacyDir(dir string) (map[string]Skill, error) {
	out := map[string]Skill{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, fmt.Errorf("read skills dir %s: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".md" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		s, err := parseLegacySkillFile(path)
		if err != nil {
			return nil, err
		}
		out[s.ID] = s
	}
	return out, nil
}

func parseLegacySkillFile(path string) (Skill, error) {
	s, err := ParseFile(path)
	if err == nil {
		return s, nil
	}
	return parseLegacyMarkdownSkill(path)
}

func parseLegacyMarkdownSkill(path string) (Skill, error) {
	data, err := os.ReadFile(path) //nolint:gosec // user-local skill directory
	if err != nil {
		return Skill{}, fmt.Errorf("read %s: %w", path, err)
	}
	id := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	body := strings.TrimSpace(string(data))
	s := Skill{
		ID:   id,
		Name: id,
		Body: body,
		Path: path,
	}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "# ") {
			s.Name = strings.TrimSpace(strings.TrimPrefix(line, "# "))
			if open := strings.LastIndex(s.Name, "(:"); open >= 0 && strings.HasSuffix(s.Name, ")") {
				s.Name = strings.TrimSpace(s.Name[:open])
			}
			break
		}
	}
	for _, para := range legacyParagraphs(body) {
		if strings.HasPrefix(para, "#") || strings.HasPrefix(para, "**") {
			continue
		}
		s.Description = para
		break
	}
	return s, nil
}

func legacyParagraphs(body string) []string {
	var out []string
	var current []string
	flush := func() {
		if len(current) == 0 {
			return
		}
		out = append(out, strings.Join(current, " "))
		current = nil
	}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			flush()
			continue
		}
		current = append(current, line)
	}
	flush()
	return out
}
