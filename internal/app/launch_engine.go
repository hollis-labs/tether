package app

import (
	"context"
	"os"

	"github.com/hollis-labs/go-agent-launch/agentlaunch"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/specresolve"
)

// LaunchEngine selects how the agentlaunch.LaunchPlan that feeds
// launcher.Compile is produced. It is the S5 platform-reshape cutover
// toggle: it changes ONLY the construction of that plan, never the
// daemon's launch.Plan bookkeeping (persistence, rehydration, provenance,
// session metadata).
type LaunchEngine string

const (
	// EngineCatalog is the default engine: the legacy bespoke
	// static-catalog walk (internal/launch.Resolve -> agentLaunchPlan).
	EngineCatalog LaunchEngine = "catalog"

	// EngineSpec is the S5 engine: the parameterized LaunchSpec resolver
	// (internal/specresolve) drives the go-agent-launch Spec engine.
	EngineSpec LaunchEngine = "spec"

	// EnvLaunchEngine is the env var that selects the launch engine. It is
	// the primary control and overrides the catalog config default.
	EnvLaunchEngine = "TETHER_LAUNCH_ENGINE"

	// EnvLaunchSpecsRoot is the env var that overrides the LaunchSpec
	// corpus directory the "spec" engine reads from.
	EnvLaunchSpecsRoot = "TETHER_LAUNCH_SPECS_ROOT"
)

// resolveLaunchEngine selects the launch engine for a catalog.
//
// Precedence (first non-empty wins):
//  1. TETHER_LAUNCH_ENGINE env var
//  2. global.yaml catalog.defaults.launch_engine
//  3. EngineCatalog (the default)
//
// Any value other than "spec" — including an unrecognized one — resolves
// to EngineCatalog. The default-off invariant: with neither the env var
// nor the config field set to "spec", the legacy catalog path is taken.
func resolveLaunchEngine(cat *config.Catalog) LaunchEngine {
	if v := os.Getenv(EnvLaunchEngine); v != "" {
		return normalizeLaunchEngine(v)
	}
	if cat != nil {
		if v := cat.Global.Catalog.Defaults.LaunchEngine; v != "" {
			return normalizeLaunchEngine(v)
		}
	}
	return EngineCatalog
}

// normalizeLaunchEngine maps a raw string onto a LaunchEngine. Only the
// exact value "spec" selects the Spec engine; everything else (including
// the empty string and typos) is the catalog engine — fail-safe to the
// behavior-preserving default.
func normalizeLaunchEngine(raw string) LaunchEngine {
	if LaunchEngine(raw) == EngineSpec {
		return EngineSpec
	}
	return EngineCatalog
}

// launchEngine reports the launch engine selected for this Service's
// catalog. It re-reads the env var on every call so a process can flip
// the toggle without a restart; the resolver itself (built over the
// registry) is still constructed once.
func (s *Service) launchEngine() LaunchEngine {
	return resolveLaunchEngine(s.Catalog)
}

// specResolverFor lazily constructs the S5 Spec-path resolver and returns
// it. Construction happens at most once per Service: it opens a
// *registry.Registry over the Service's catalog root and wraps it in a
// *specresolve.Resolver pointed at the configured specsRoot. Both are
// concurrency-safe after construction and reused across every launch.
//
// It is only ever called on the "spec" path; the "catalog" path never
// touches it, so a Service running the default engine pays no cost.
func (s *Service) specResolverFor() (*specresolve.Resolver, error) {
	s.specResolverOnce.Do(func() {
		reg, err := registry.OpenAt(registry.Options{CatalogRoot: s.CatalogRoot})
		if err != nil {
			s.specResolverErr = err
			return
		}
		var opts []specresolve.Option
		if root := resolveLaunchSpecsRoot(s.Catalog); root != "" {
			opts = append(opts, specresolve.WithSpecsRoot(root))
		}
		s.specResolver, s.specResolverErr = specresolve.NewResolver(reg, opts...)
	})
	return s.specResolver, s.specResolverErr
}

// LaunchEngineIsSpec reports whether the S5 "spec" launch engine is
// selected for this Service. It is the public read of the toggle that
// front-ends outside internal/app (e.g. the boot-exec CLI) branch on.
func (s *Service) LaunchEngineIsSpec() bool {
	return s.launchEngine() == EngineSpec
}

// SpecResolveLaunchPlan resolves launchID to an agentlaunch.LaunchPlan via
// the S5 Spec engine (internal/specresolve). It is the public entry point
// for front-ends that produce the agentlaunch.LaunchPlan themselves
// (boot-exec, mux resolve) rather than going through compileSharedLaunch.
//
// frontEnd selects missing-required-input handling and the stamped launch
// mode: pass agentlaunch.FrontEndInteractive for the interactive
// terminal-attached front-ends. The returned plan is Validate()-clean.
//
// Callers must only invoke this when LaunchEngineIsSpec reports true; it
// constructs the resolver lazily and is a no-op cost on the catalog path.
func (s *Service) SpecResolveLaunchPlan(ctx context.Context, launchID string, frontEnd agentlaunch.RenderFrontEnd) (agentlaunch.LaunchPlan, error) {
	resolver, err := s.specResolverFor()
	if err != nil {
		return agentlaunch.LaunchPlan{}, err
	}
	return resolver.ResolveContext(ctx, launchID, frontEnd)
}

// resolveLaunchSpecsRoot selects the LaunchSpec corpus directory for the
// "spec" engine.
//
// Precedence (first non-empty wins):
//  1. TETHER_LAUNCH_SPECS_ROOT env var
//  2. global.yaml catalog.defaults.launch_specs_root
//  3. "" — NewResolver then falls back to <home>/.tether/launch-specs
//     (specresolve.DefaultSpecsRoot).
//
// A non-empty result has ~ expansion applied; an empty result is returned
// verbatim so the caller can let specresolve apply its own default.
func resolveLaunchSpecsRoot(cat *config.Catalog) string {
	if v := os.Getenv(EnvLaunchSpecsRoot); v != "" {
		return config.Expand(v)
	}
	if cat != nil {
		if v := cat.Global.Catalog.Defaults.LaunchSpecsRoot; v != "" {
			return config.Expand(v)
		}
	}
	return ""
}
