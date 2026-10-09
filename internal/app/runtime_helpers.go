package app

import "github.com/hollis-labs/tether/internal/apikeyhelper"

// ResolveAPIKeyHelperPath returns an absolute path to the tether-apikey-helper
// binary. Resolution order: $TETHER_APIKEY_HELPER env override, then a sibling
// next to the tether binary, then $PATH lookup. Returns empty when not found.
func ResolveAPIKeyHelperPath() string {
	return apikeyhelper.ResolvePath()
}

func resolveAPIKeyHelperPath() string { return ResolveAPIKeyHelperPath() }
