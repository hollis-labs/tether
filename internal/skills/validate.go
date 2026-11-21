package skills

import (
	"fmt"
	"sort"

	"github.com/hollis-labs/tether/internal/config"
)

// ValidateCatalog checks every skill in the launcher's discovery layers,
// including unused and shadowed files, then checks agent skill references.
// It deliberately uses ParseFile, just like loadEffectiveSkills at launch.
func ValidateCatalog(root string, cat *config.Catalog) error {
	available := map[string]bool{}
	for _, layer := range config.DefaultLayers(root, root) {
		discovered, err := config.Discover([]config.LayerSpec{layer})
		if err != nil {
			return fmt.Errorf("discover catalog skills: %w", err)
		}
		ids := make([]string, 0, len(discovered.SkillPaths))
		for id := range discovered.SkillPaths {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			path := discovered.SkillPaths[id].Path
			if _, err := ParseFile(path); err != nil {
				return fmt.Errorf("catalog skill %q: %w; fix %s: use YAML frontmatter delimited by --- with a non-empty id (see docs/skill-format.md)", id, err, path)
			}
			available[id] = true
		}
	}
	ids := make([]string, 0, len(cat.Agents))
	for id := range cat.Agents {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		for _, skill := range cat.Agents[id].Skills {
			if !available[skill] {
				return fmt.Errorf("agent %q references missing skill %q; add skills/%s.md with valid YAML frontmatter and id, or remove it from the agent's skills list (see docs/skill-format.md)", id, skill, skill)
			}
		}
	}
	return nil
}
