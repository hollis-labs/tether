package mcpgateway

// ResourceAllowed applies the endpoint's origin boundary and every profile
// floor. Tool-restricted profiles may read only resources explicitly referenced
// by an eligible tool's MCP Apps metadata. Tool names are not resource names;
// treating an arbitrary resource as a read-only tool would bypass tool denies.
// Callers must also enforce the credential's origin grant and availability.
func (p Policy) ResourceAllowed(origin, uri string, snapshot Snapshot) bool {
	if !p.ResourceOriginAllowed(origin) {
		return false
	}
	restricted := false
	for _, selection := range append([]ProfileSelection{p.Selection}, p.Floors...) {
		if profile := selection.Profile; profile != nil && (profile.Tools.Allow != nil || len(profile.Tools.Deny) > 0) {
			restricted = true
		}
	}
	referenced := false
	for _, entry := range snapshot.Entries {
		if entry.Origin != origin || entry.Tool == nil {
			continue
		}
		ui, _ := entry.Tool.Meta["ui"].(map[string]any)
		if ui["resourceUri"] != uri && entry.Tool.Meta["ui/resourceUri"] != uri {
			continue
		}
		referenced = true
		if p.Exclusion(entry) == "" {
			return true
		}
	}
	return !restricted && !referenced
}

// ResourceOriginAllowed checks origin restrictions before any discovery RPC.
func (p Policy) ResourceOriginAllowed(origin string) bool {
	if !p.originSelected(origin) {
		return false
	}
	for _, id := range p.RestrictedOrigins {
		if id == origin {
			return false
		}
	}
	return true
}
