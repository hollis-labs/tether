package mcpgateway

import (
	"fmt"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"path"
	"sort"
	"unicode/utf8"
)

func (p Profile) Validate() error {
	if p.DiscoveryMode != nil {
		if err := ValidateMode(*p.DiscoveryMode); err != nil {
			return err
		}
	}
	if utf8.RuneCountInString(p.Instructions) > 2048 {
		return fmt.Errorf("instructions exceeds 2048 characters")
	}
	for _, patterns := range [][]string{p.Tools.Allow, p.Tools.Deny} {
		for _, pattern := range patterns {
			if _, err := path.Match(pattern, ""); err != nil {
				return fmt.Errorf("invalid tool glob %q: %w", pattern, err)
			}
		}
	}
	for _, names := range [][]string{p.Servers, p.Order, p.AlwaysLoad} {
		seen := map[string]bool{}
		for _, name := range names {
			if name == "" || seen[name] {
				return fmt.Errorf("empty or duplicate profile name %q", name)
			}
			seen[name] = true
		}
	}
	return nil
}

type ProfileInputs struct {
	Explicit    []Selector
	Environment *string
}
type ProfileSelection struct {
	ID      string   `json:"profile,omitempty"`
	Source  string   `json:"profile_source,omitempty"`
	Profile *Profile `json:"profile_config,omitempty"`
}

func ResolveProfile(config Config, in ProfileInputs) (ProfileSelection, error) {
	var candidates []Selector
	for i, selector := range in.Explicit {
		if i > 0 && selector.Value != in.Explicit[0].Value {
			return ProfileSelection{}, fmt.Errorf("conflicting explicit profile selectors")
		}
		candidates = append(candidates, selector)
	}
	if in.Environment != nil {
		candidates = append(candidates, Selector{*in.Environment, "environment"})
	}
	for _, candidate := range candidates {
		if candidate.Value == "" {
			return ProfileSelection{}, fmt.Errorf("%s: empty MCP profile", candidate.Source)
		}
		if _, ok := config.Profiles[candidate.Value]; !ok {
			return ProfileSelection{}, fmt.Errorf("%s: unknown MCP profile %q", candidate.Source, candidate.Value)
		}
	}
	if err := config.Validate(); err != nil {
		return ProfileSelection{}, err
	}
	if len(candidates) == 0 {
		return ProfileSelection{}, nil
	}
	selected := candidates[0]
	profile := config.Profiles[selected.Value]
	return ProfileSelection{selected.Value, selected.Source, &profile}, nil
}

// Policy is immutable endpoint policy. Upstream order is supplied by the loader;
// native tools are otherwise ordered with origin tether before catalog origins.
type Policy struct {
	Selection   ProfileSelection
	ServerOrder []string
}

func (p Policy) Exclusion(entry Entry) string {
	profile := p.Selection.Profile
	if profile == nil {
		return ""
	}
	if profile.Servers != nil {
		found := false
		for _, id := range profile.Servers {
			if id == entry.Origin {
				found = true
				break
			}
		}
		if !found {
			return "profile servers"
		}
	}
	matches := func(patterns []string) bool {
		for _, pattern := range patterns {
			if yes, _ := path.Match(pattern, entry.Tool.Name); yes {
				return true
			}
		}
		return false
	}
	if matches(profile.Tools.Deny) {
		return "profile deny"
	}
	if profile.ReadOnly && (entry.Tool.Annotations == nil || !entry.Tool.Annotations.ReadOnlyHint) {
		return "profile read_only requires readOnlyHint=true"
	}
	if profile.Tools.Allow != nil && !matches(profile.Tools.Allow) {
		return "profile allow"
	}
	return ""
}
func (p Policy) Decorate(entry Entry) Entry {
	profile := p.Selection.Profile
	if profile == nil {
		return entry
	}
	for _, name := range profile.AlwaysLoad {
		if name == entry.Tool.Name {
			tool := *entry.Tool
			tool.Meta = mcpsdk.Meta{}
			for k, v := range entry.Tool.Meta {
				tool.Meta[k] = v
			}
			tool.Meta["anthropic/alwaysLoad"] = true
			entry.Tool = &tool
			break
		}
	}
	return entry
}
func (p Policy) Less(a, b Entry) bool {
	pins := []string{}
	if p.Selection.Profile != nil {
		pins = p.Selection.Profile.Order
	}
	position := func(name string, names []string) int {
		for i, v := range names {
			if v == name {
				return i
			}
		}
		return len(names)
	}
	ai, bi := position(a.Tool.Name, pins), position(b.Tool.Name, pins)
	if ai != bi {
		return ai < bi
	}
	ai, bi = position(a.Origin, p.ServerOrder), position(b.Origin, p.ServerOrder)
	if ai != bi {
		return ai < bi
	}
	return a.Tool.Name < b.Tool.Name
}
func (p Policy) Eligible(snapshot Snapshot) Snapshot {
	out := Snapshot{Origins: snapshot.Origins, Entries: []Entry{}}
	for _, entry := range snapshot.Entries {
		if p.Exclusion(entry) == "" {
			out.Entries = append(out.Entries, p.Decorate(entry))
		}
	}
	sort.Slice(out.Entries, func(i, j int) bool { return p.Less(out.Entries[i], out.Entries[j]) })
	return out
}
func (p Policy) ValidateNames(snapshot Snapshot) error {
	profile := p.Selection.Profile
	if profile == nil {
		return nil
	}
	known := map[string]bool{}
	for _, entry := range snapshot.Entries {
		known[entry.Tool.Name] = true
	}
	for _, names := range [][]string{profile.Order, profile.AlwaysLoad} {
		for _, name := range names {
			if !known[name] {
				return fmt.Errorf("profile %q references unknown order/always_load tool %q", p.Selection.ID, name)
			}
		}
	}
	return nil
}

// SelectOrigins validates every explicitly named origin before intersecting
// profile origins with the upstream-only CLI restriction. No secrets are read.
func SelectOrigins(known map[string]bool, upstreamRestriction []string, profile *Profile) ([]string, error) {
	for _, id := range upstreamRestriction {
		if id == "tether" || !known[id] {
			return nil, fmt.Errorf("unknown or disabled MCP server ID %q", id)
		}
	}
	if profile == nil || profile.Servers == nil {
		return upstreamRestriction, nil
	}
	for _, id := range profile.Servers {
		if !known[id] {
			return nil, fmt.Errorf("unknown or disabled MCP profile origin %q", id)
		}
	}
	selected := []string{}
	for _, id := range profile.Servers {
		if id == "tether" {
			continue
		}
		permitted := upstreamRestriction == nil
		for _, allowed := range upstreamRestriction {
			if allowed == id {
				permitted = true
				break
			}
		}
		if permitted {
			selected = append(selected, id)
		}
	}
	return selected, nil
}
