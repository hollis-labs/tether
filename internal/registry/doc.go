// Package registry is Tether's federation directory service. It owns
// public-identity rows for agents and projects (v060-01); the owning
// substrate retains all operational configuration behind a callback URI.
// The two-store model — Mux for discovery, substrate for ops — is the
// load-bearing design choice of the v0.6 epic. See ADR 0041 for the
// full rationale.
//
// Package layout. The pre-v060-01 internal/registry/ package held a
// different concern (launch resolution over ~/.tether/catalog/*). That
// package was renamed to internal/launchresolve/ in T-v060-01-01 to free
// the registry name for this service. D14 mandates "registry" as the
// user-facing term (HTTP path /registry/*, MCP tool prefix
// tether_registry_*, CLI subcommand mux registry); aligning the internal
// package name removes a cognitive-drift risk for readers comparing API
// surface to source. The launchresolve package is otherwise untouched —
// different concern, no shared types.
//
// What's here.
//
//   - model.go — public-identity types: Profile, Skill, Link, Callback,
//     Filter, ArrayPatch + ArrayMode. These are BOTH storage-row mirrors
//     and HTTP/MCP API envelopes; the two stores share the same shape.
//   - id.go — URN minter. Stripe-style opaque IDs (D2): agt_<10alnum>,
//     prj_<10alnum>. crypto/rand source, rejection-sampled across a
//     36-character alphabet to keep the distribution unbiased.
//
// Subsequent v060-01 tasks land service.go (T-v060-01-03), storage.go
// (T-v060-01-02), callback.go (T-v060-01-04), and bootstrap.go
// (T-v060-01-08). The v060-02 sprint adds the cerberus importer and
// cross-substrate dedup primitive.
package registry
