// Package specresolve is Tether's Spec-path launch resolver for the S5
// platform-reshape cutover. It resolves a launch id to a runnable
// go-agent-launch agentlaunch.LaunchPlan by driving the parameterized
// LaunchSpec engine instead of the legacy bespoke static-catalog walk.
//
// The resolution pipeline this package implements:
//
//	launch id
//	  -> load LaunchBag + LaunchSpec from the spec corpus
//	  -> ValidateMinimumConfig
//	  -> S4.2 var resolution (file / cmd / call sources, trust-gated)
//	  -> LaunchSpec.Render against the bag
//	  -> resolve runner -> registry.ResolveRuntimeBinding
//	  -> resolve agent  -> registry.ResolveAgent
//	  -> agentlaunch.PlanFromLaunch
//	  -> agentlaunch.LaunchPlan
//
// The returned LaunchPlan is Validate()-clean and feeds the already-shipped
// launcher.Compile -> Prepare -> Plant pipeline. This package stops at
// producing the plan; it does not run it.
//
// This package builds ONLY the resolution layer. It deliberately does not
// wire itself into any launch front-end, the daemon, internal/app, or
// internal/launch — that is a later, gated step. The Resolver is the new
// Spec-path counterpart of the legacy internal/launch resolver.
//
// Locked design constraints honored here:
//
//   - D1 — local-first / offline. Resolution works fully offline. The only
//     network is the var-resolution http `call` sources (the Tesseract
//     recall endpoint); the spec corpus authors those sources with
//     on_error: warn so a recall-endpoint-down condition degrades the
//     rendered var to its fallback (or empty) and the launch still
//     resolves. Runner/agent resolution is delegated to internal/registry,
//     whose DegradingRegistrar serves a last-known-good cache when the
//     directory is down.
//
//   - D6(c) — gated var sources. The corpus's call/cmd var sources each
//     carry a TrustGate. The Resolver supplies a TrustAuthorizer that
//     authorizes exactly the catalog-authored trust tokens (see
//     catalogTrustTokens) and an http CallResolver; an unrecognized token
//     is denied fail-closed.
//
// Catalog-shape note. Runner and agent resolution is delegated to
// internal/registry, which hand-maps Tether-native catalog YAML into the
// go-agent-launch return types. See that package for detail.
package specresolve
