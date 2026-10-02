package mcpgateway

import (
	"fmt"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"regexp"
	"slices"
	"sort"
	"strings"
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
			if _, err := matchToolGlob(pattern, ""); err != nil {
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
	if len(candidates) == 0 {
		return ProfileSelection{}, nil
	}
	selected := candidates[0]
	profile := config.Profiles[selected.Value]
	if err := profile.Validate(); err != nil {
		return ProfileSelection{}, fmt.Errorf("mcp.profiles.%s: %w", selected.Value, err)
	}
	return ProfileSelection{selected.Value, selected.Source, &profile}, nil
}

// Policy is immutable endpoint policy. Upstream order is supplied by the loader;
// native tools are otherwise ordered with origin tether before catalog origins.
type Policy struct {
	Selection         ProfileSelection
	Floors            []ProfileSelection `json:"authority_profiles,omitempty"`
	ServerOrder       []string
	RestrictedOrigins []string `json:"restricted_origins,omitempty"`
}

func (p Policy) Exclusion(entry Entry) string {
	for _, floor := range p.Floors {
		if reason := (Policy{Selection: floor}).Exclusion(entry); reason != "" {
			return "launch profile: " + reason
		}
	}
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
			if yes, _ := matchToolGlob(pattern, entry.Tool.Name); yes {
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

// CloneProfile preserves nil/empty rules while owning all mutable selectors.
func CloneProfile(p Profile) Profile {
	p.Servers = slices.Clone(p.Servers)
	p.Tools.Allow = slices.Clone(p.Tools.Allow)
	p.Tools.Deny = slices.Clone(p.Tools.Deny)
	p.Order = slices.Clone(p.Order)
	p.AlwaysLoad = slices.Clone(p.AlwaysLoad)
	if p.DiscoveryMode != nil {
		value := *p.DiscoveryMode
		p.DiscoveryMode = &value
	}
	return p
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
			if IsInfrastructure(name) {
				return fmt.Errorf("profile order/always_load cannot reference gateway infrastructure %q", name)
			}
			if !known[name] && len(p.NameWarnings(snapshot)) == 0 {
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
		if !known[id] {
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

// NameWarnings defer unknown-pin checks when discovery is incomplete. Final
// wire names need not encode their origin, so an unknown pin cannot safely be
// attributed to a connected origin while another upstream is unavailable.
func (p Policy) NameWarnings(snapshot Snapshot) []string {
	if p.Selection.Profile == nil {
		return nil
	}
	known := map[string]string{}
	for _, e := range snapshot.Entries {
		known[e.Tool.Name] = e.Origin
	}
	missing := false
	pinnedOrigins := map[string]bool{}
	for _, names := range [][]string{p.Selection.Profile.Order, p.Selection.Profile.AlwaysLoad} {
		for _, name := range names {
			origin, exists := known[name]
			if !exists && !IsInfrastructure(name) {
				missing = true
			}
			if exists {
				pinnedOrigins[origin] = true
			}
		}
	}
	var warnings []string
	for _, origin := range snapshot.Origins {
		if origin.ID != "tether" && origin.Status != "connected" && origin.Status != "excluded" && (missing || pinnedOrigins[origin.ID]) {
			warnings = append(warnings, fmt.Sprintf("upstream %s unavailable; profile order/always_load validation deferred until discovery recovers", origin.ID))
		}
	}
	return warnings
}

func IsInfrastructure(name string) bool {
	switch name {
	case "tether_gateway_status", "tether_tool_search", "tether_tool_list", "tether_tool_call":
		return true
	}
	return false
}

// matchToolGlob matches entire tool names, not filesystem paths. Every rune,
// including '/', participates in '*' and '?'. Character classes and escapes
// retain the documented glob syntax.
func matchToolGlob(pattern, name string) (bool, error) {
	var out strings.Builder
	out.WriteString("(?s)^")
	runes := []rune(pattern)
	literal := func(r rune) { fmt.Fprintf(&out, `\x{%x}`, r) }
	for i := 0; i < len(runes); i++ {
		switch r := runes[i]; r {
		case '*':
			out.WriteString(".*")
		case '?':
			out.WriteByte('.')
		case '\\':
			i++
			if i == len(runes) {
				return false, fmt.Errorf("trailing escape")
			}
			literal(runes[i])
		case '[':
			out.WriteByte('[')
			i++
			if i < len(runes) && runes[i] == '^' {
				out.WriteByte('^')
				i++
			}
			count := 0
			for ; i < len(runes) && runes[i] != ']'; i++ {
				switch runes[i] {
				case '\\':
					i++
					if i == len(runes) {
						return false, fmt.Errorf("trailing class escape")
					}
					literal(runes[i])
				case '-':
					out.WriteByte('-')
				default:
					literal(runes[i])
				}
				count++
			}
			if i == len(runes) || count == 0 {
				return false, fmt.Errorf("unterminated or empty character class")
			}
			out.WriteByte(']')
		default:
			literal(r)
		}
	}
	out.WriteByte('$')
	re, err := regexp.Compile(out.String())
	if err != nil {
		return false, err
	}
	return re.MatchString(name), nil
}
