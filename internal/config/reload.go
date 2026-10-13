package config

import (
	"reflect"
	"sort"
)

// ReloadReport says what a catalog reload changed (CW-20261013-0002).
type ReloadReport struct {
	// Reloaded is true when the new catalog is now the one launches use.
	Reloaded bool `json:"reloaded"`
	// Added, Removed and Changed list entity ids per kind (agents, launches,
	// projects, providers, sandbox_profiles).
	Added   map[string][]string `json:"added,omitempty"`
	Removed map[string][]string `json:"removed,omitempty"`
	Changed map[string][]string `json:"changed,omitempty"`
	// RestartRequired names edits a reload does not apply: the daemon's own
	// settings in global.yaml (listener, identity, AI providers, MCP gateway,
	// modules) are read once at start.
	RestartRequired []string `json:"restart_required,omitempty"`
}

// Empty reports whether the reload changed nothing.
func (r ReloadReport) Empty() bool {
	return len(r.Added) == 0 && len(r.Removed) == 0 && len(r.Changed) == 0 && len(r.RestartRequired) == 0
}

// DiffCatalogs compares the entity maps of two catalogs. GlobalChanged is
// reported through RestartRequired by the caller, which also decides what
// global.yaml the swapped catalog keeps.
func DiffCatalogs(oldCat, newCat *Catalog) ReloadReport {
	r := ReloadReport{Added: map[string][]string{}, Removed: map[string][]string{}, Changed: map[string][]string{}}
	if oldCat == nil || newCat == nil {
		return r
	}
	diffMap(&r, "agents", oldCat.Agents, newCat.Agents)
	diffMap(&r, "launches", oldCat.Launches, newCat.Launches)
	diffMap(&r, "projects", oldCat.Projects, newCat.Projects)
	diffMap(&r, "providers", oldCat.Providers, newCat.Providers)
	diffMap(&r, "sandbox_profiles", oldCat.SandboxProfiles, newCat.SandboxProfiles)
	for _, m := range []map[string][]string{r.Added, r.Removed, r.Changed} {
		for k, v := range m {
			sort.Strings(v)
			if len(v) == 0 {
				delete(m, k)
			}
		}
	}
	return r
}

func diffMap[V any](r *ReloadReport, kind string, a, b map[string]V) {
	for id, bv := range b {
		av, ok := a[id]
		switch {
		case !ok:
			r.Added[kind] = append(r.Added[kind], id)
		case !reflect.DeepEqual(av, bv):
			r.Changed[kind] = append(r.Changed[kind], id)
		}
	}
	for id := range a {
		if _, ok := b[id]; !ok {
			r.Removed[kind] = append(r.Removed[kind], id)
		}
	}
}

// GlobalDiffers reports whether the global.yaml settings differ.
func GlobalDiffers(a, b *Catalog) bool {
	return a != nil && b != nil && !reflect.DeepEqual(a.Global, b.Global)
}
