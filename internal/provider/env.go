package provider

import (
	"sort"
	"strings"
)

// EnvMode selects how a provider composes a child process's environment.
//
// "merge" (the default) inherits the parent environment, drops any keys in
// the redact list, and overlays explicit overrides. It is the sane default
// for local developer workflows where the child needs $PATH, $HOME, $SHELL,
// and similar without the catalog author having to list every key.
//
// "whitelist" starts from an empty environment, copies only the keys named
// in passthrough from the parent, and overlays overrides. It is opt-in for
// isolation-sensitive providers where leaking parent state is undesirable.
const (
	EnvModeMerge     = "merge"
	EnvModeWhitelist = "whitelist"
)

// BuildEnv composes the effective child environment from a provider's env
// policy. The policy pieces come from the launch plan (mode, passthrough,
// redact, overrides); parent is the current process's environment, typically
// os.Environ() at session start.
//
// Unknown or empty mode strings are treated as EnvModeMerge — merge is the
// safer default for accidental omissions (the child just inherits too much,
// which surfaces loudly; a whitelist omission would silently strip required
// vars and make launches fail in confusing ways).
//
// The returned slice is sorted by key for test determinism.
func BuildEnv(mode string, passthrough, redact []string, overrides map[string]string, parent []string) []string {
	parentMap := parseEnv(parent)

	var base map[string]string
	switch mode {
	case EnvModeWhitelist:
		base = make(map[string]string, len(passthrough))
		for _, k := range passthrough {
			if v, ok := parentMap[k]; ok {
				base[k] = v
			}
		}
	default: // EnvModeMerge, "", anything else
		base = make(map[string]string, len(parentMap))
		for k, v := range parentMap {
			base[k] = v
		}
		for _, k := range redact {
			delete(base, k)
		}
	}

	for k, v := range overrides {
		base[k] = v
	}

	keys := make([]string, 0, len(base))
	for k := range base {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(base))
	for _, k := range keys {
		out = append(out, k+"="+base[k])
	}
	return out
}

// parseEnv splits os.Environ()-style `KEY=VALUE` entries into a map. Entries
// without a `=` are ignored. The first `=` delimits the key, so values may
// contain further `=` characters (e.g., URLs with query strings).
func parseEnv(entries []string) map[string]string {
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		i := strings.IndexByte(e, '=')
		if i <= 0 {
			continue
		}
		out[e[:i]] = e[i+1:]
	}
	return out
}
