package app

import "github.com/hollis-labs/tether/internal/apikeyhelper"

// providerRecordsSessionID reports whether the provider emits a
// provider-side session id worth persisting for crash-recovery flows.
// Normal launches do not consume the stored id; `--resume` is reserved for
// an explicit recovery path.
func providerRecordsSessionID(providerID string) bool {
	switch providerID {
	case "claude-code", "claude-stream", "claude-goprovider", "claude-pty", "opencode":
		return true
	}
	return false
}

// ResolveAPIKeyHelperPath returns an absolute path to the mux-apikey-helper
// binary. Resolution order: $MUX_APIKEY_HELPER env override, then a sibling
// next to the mux binary, then $PATH lookup. Returns empty when not found.
func ResolveAPIKeyHelperPath() string {
	return apikeyhelper.ResolvePath()
}

func resolveAPIKeyHelperPath() string { return ResolveAPIKeyHelperPath() }
