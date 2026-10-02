// Package mcpgateway owns discovery selection and the semantic inventory API.
// Transports supply inventory/dispatch seams; they do not choose discovery policy.
package mcpgateway

import "fmt"

type Mode string

const (
	Flat   Mode = "flat"
	Search Mode = "search"
)

func ValidateMode(value string) error {
	if value != string(Flat) && value != string(Search) {
		return fmt.Errorf("unknown MCP discovery mode %q; expected flat or search", value)
	}
	return nil
}

// Profile is the typed mode hook for profile selection (CW-20260926-0008).
// This change does not select or filter profiles.
type Profile struct {
	DiscoveryMode *string `yaml:"discovery_mode" json:"discovery_mode,omitempty"`
}

type Config struct {
	DiscoveryMode *string            `yaml:"discovery_mode" json:"discovery_mode,omitempty"`
	Profiles      map[string]Profile `yaml:"profiles" json:"profiles,omitempty"`
}

func (c Config) Validate() error {
	if c.DiscoveryMode != nil {
		if err := ValidateMode(*c.DiscoveryMode); err != nil {
			return fmt.Errorf("mcp.discovery_mode: %w", err)
		}
	}
	for id, profile := range c.Profiles {
		if profile.DiscoveryMode != nil {
			if err := ValidateMode(*profile.DiscoveryMode); err != nil {
				return fmt.Errorf("mcp.profiles.%s.discovery_mode: %w", id, err)
			}
		}
	}
	return nil
}

type Selector struct{ Value, Source string }
type ModeInputs struct {
	Explicit    []Selector
	Environment *string
	Profile     *Profile
	Gateway     *string
	Setting     *string
}
type Selection struct {
	Mode   Mode   `json:"mode"`
	Source string `json:"source"`
}

// ResolveMode validates ALL supplied tiers, including overridden values. Empty
// is a supplied value, never omission. Explicit selectors at one tier must agree.
func ResolveMode(in ModeInputs) (Selection, error) {
	candidates := append([]Selector(nil), in.Explicit...)
	for i, selector := range in.Explicit {
		if err := ValidateMode(selector.Value); err != nil {
			return Selection{}, fmt.Errorf("%s: %w", selector.Source, err)
		}
		if i > 0 && selector.Value != in.Explicit[0].Value {
			return Selection{}, fmt.Errorf("conflicting explicit discovery-mode selectors: %s and %s", in.Explicit[0].Source, selector.Source)
		}
	}
	if in.Environment != nil {
		candidates = append(candidates, Selector{*in.Environment, "environment"})
	}
	if in.Profile != nil && in.Profile.DiscoveryMode != nil {
		candidates = append(candidates, Selector{*in.Profile.DiscoveryMode, "profile"})
	}
	if in.Gateway != nil {
		candidates = append(candidates, Selector{*in.Gateway, "gateway_config"})
	}
	if in.Setting != nil {
		candidates = append(candidates, Selector{*in.Setting, "tether_setting"})
	}
	for _, candidate := range candidates {
		if err := ValidateMode(candidate.Value); err != nil {
			return Selection{}, fmt.Errorf("%s: %w", candidate.Source, err)
		}
	}
	if len(candidates) == 0 {
		return Selection{Flat, "default"}, nil
	}
	return Selection{Mode(candidates[0].Value), candidates[0].Source}, nil
}
