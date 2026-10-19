package catalogapply

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/hollis-labs/tether/internal/teamspolicy"
	"gopkg.in/yaml.v3"
)

// TeamActivationRequest changes one existing binding, never the surrounding
// teams policy. It only creates a normal review plan: approval/apply/restart
// remain separate provider operations.
type TeamActivationRequest struct {
	Name       string          `json:"name"`
	TaskID     string          `json:"task_id,omitempty"`
	LaunchID   string          `json:"launch_id"`
	Expected   teamspolicy.Pin `json:"expected_pin"`
	Definition teamspolicy.Pin `json:"definition"`
	Source     string          `json:"source"`
}

func (e *Engine) PlanTeamActivation(ctx context.Context, req TeamActivationRequest, opt Options) (*Plan, error) {
	raw, err := e.ReadFile(teamspolicy.FileName)
	if err != nil {
		return nil, err
	}
	policy, err := teamspolicy.Parse([]byte(raw))
	if err != nil {
		return nil, err
	}
	found := false
	for _, l := range policy.Launches() {
		if l.Launch == req.LaunchID {
			if l.Definition != req.Expected {
				return nil, errors.New("team binding changed; inspect and replan")
			}
			found = true
		}
	}
	if !found {
		return nil, errors.New("existing team launch binding required")
	}
	if req.Definition.ID != req.Expected.ID || req.Definition.Revision == "" || req.Definition.Digest == "" {
		return nil, errors.New("exact revision of the same definition required")
	}
	if len(req.Source) <= len("catalog:") || req.Source[:len("catalog:")] != "catalog:" {
		return nil, errors.New("catalog definition source required")
	}
	source := req.Source[len("catalog:"):]
	if len(source) <= len("team-definitions/") || source[:len("team-definitions/")] != "team-definitions/" {
		return nil, errors.New("team-definitions source required")
	}
	definition, err := e.ReadFile(source)
	if err != nil {
		return nil, err
	}
	id, rev, digest, err := teamspolicy.PinOfBytes([]byte(definition))
	if err != nil {
		return nil, err
	}
	if req.Definition != (teamspolicy.Pin{ID: id, Revision: rev, Digest: digest}) {
		return nil, errors.New("installed definition does not match proposed pin")
	}
	var node yaml.Node
	if err = yaml.Unmarshal([]byte(raw), &node); err != nil {
		return nil, err
	}
	root := node.Content[0]
	for n := 0; n < len(root.Content); n += 2 {
		if root.Content[n].Value != "launches" {
			continue
		}
		for _, entry := range root.Content[n+1].Content {
			match := false
			for k := 0; k < len(entry.Content); k += 2 {
				if entry.Content[k].Value == "launch" && entry.Content[k+1].Value == req.LaunchID {
					match = true
				}
			}
			if !match {
				continue
			}
			var pin yaml.Node
			if err = pin.Encode(req.Definition); err != nil {
				return nil, err
			}
			sourceSet := false
			for k := 0; k < len(entry.Content); k += 2 {
				switch entry.Content[k].Value {
				case "definition":
					entry.Content[k+1] = &pin
				case "source":
					entry.Content[k+1].Value = req.Source
					sourceSet = true
				}
			}
			if !sourceSet {
				entry.Content = append(entry.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "source"}, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: req.Source})
			}
		}
	}
	updated, err := yaml.Marshal(&node)
	if err != nil {
		return nil, err
	}
	if _, err = teamspolicy.Parse(updated); err != nil {
		return nil, fmt.Errorf("updated team binding: %w", err)
	}
	opt.expectedBase = map[string]string{teamspolicy.FileName: sha(raw), source: sha(definition)}
	// Include the source unchanged to bind its exact reviewed bytes to Apply's
	// existing optimistic concurrency checks as well as the policy CAS.
	return e.PlanBundle(ctx, Bundle{Name: req.Name, TaskID: req.TaskID, Files: []File{{Path: teamspolicy.FileName, Content: string(updated)}, {Path: source, Content: definition}}}, opt)
}

func (e *Engine) teamLaunchReasons(path string) []string {
	if !strings.HasPrefix(path, "launches/") {
		return nil
	}
	policy, err := teamspolicy.Load(e.Root)
	if err != nil {
		return nil
	}
	id := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	for _, l := range policy.Launches() {
		if l.Launch == id {
			return []string{"changes launch behavior mapped by team definition pin; evidence may report launch_changed_since_load"}
		}
	}
	return nil
}
