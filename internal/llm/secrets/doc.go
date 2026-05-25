// Package secrets resolves runtime-only AI provider secret references.
//
// The package accepts secret references such as keychain://openai/personal and
// helper://mux-apikey-helper/openai/default, invokes the configured helper
// just-in-time, and returns secret material without persisting it to Tether
// state.
package secrets
