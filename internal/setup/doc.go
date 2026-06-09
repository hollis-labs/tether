// Package setup provides shared onboarding primitives for the beta install
// story: a provider-detection helper that reuses go-providers adapter Detect()
// plumbing, and the embedded starter catalog used by both the daemon auto-seed
// path (T-v06x-01-02) and the mux init guided setup (T-v06x-01-03).
//
// The detection helper (DetectProviders) surfaces found/missing status for
// claude, codex, and opencode without standing up a full launch plan. The
// seeding helper (WriteCatalog) writes a minimal or full starter catalog to a
// state-root directory, creating the catalog/state/run/logs subdirectories the
// daemon expects, and is safe to re-run (idempotent by default).
package setup
