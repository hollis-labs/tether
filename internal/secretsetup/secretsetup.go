// Package secretsetup builds Tether's single go-secrets Resolver.
//
// Tether has no resolver of its own: this package only supplies the
// Tether-specific configuration (the legacy "tether" keychain location and the
// conventional environment fallbacks) to github.com/hollis-labs/go-secrets.
package secretsetup

import (
	"strings"

	secrets "github.com/hollis-labs/go-secrets"
)

// LegacyService is the keychain service tether-apikey-helper used before the
// shared "hollis-labs" service. Entries there stay readable.
const LegacyService = "tether"

// LegacyAccount maps a reference to the account tether-apikey-helper used:
// provider-api-key:<authority>/<path>. Empty when the reference has no
// location in the legacy scheme.
func LegacyAccount(ref secrets.Ref) string {
	if ref.Authority == "" || len(ref.Path) == 0 {
		return ""
	}
	return "provider-api-key:" + ref.Key()
}

// EnvNames lists the environment variables tried when the keychain has no
// entry (or is unavailable). It is not a first choice: the entry wins.
func EnvNames(ref secrets.Ref) []string {
	parts := append([]string{ref.Authority}, ref.Path...)
	names := []string{"TETHER_APIKEY_" + normalizeEnvSuffix(strings.Join(parts, "_"))}
	switch ref.Authority {
	case "openai":
		names = append(names, "OPENAI_API_KEY")
	case "anthropic":
		names = append(names, "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN")
	}
	return names
}

func normalizeEnvSuffix(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - ('a' - 'A'))
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// New returns Tether's resolver. Extra options (a redactor, a test store) are
// applied after Tether's defaults.
func New(opts ...secrets.Option) *secrets.Resolver {
	base := []secrets.Option{
		secrets.WithLegacy(LegacyService, LegacyAccount),
		secrets.WithEnvFallback(EnvNames),
	}
	return secrets.New(append(base, opts...)...)
}
