// Package launchparity drives the go-agent-launch S4.5 parity harness
// over Tether's FULL launch corpus.
//
// parity.RunParity's built-in Corpus is the 11-entry S4.4 sample.
// Tether's cutover gate (EP-20260516-0001 S5.1) needs parity proven over
// all 64 legacy launches, so this test passes the full corpus via
// parity.WithCorpus and registers Tether's documented legacy-catalog
// defects via parity.WithExpectedOldErrors (go-agent-launch v0.3.5+ —
// caller-side expected registries; no hand-editing of the harness).
//
// Parity proves launch *identity* (project / work_dir / runner /
// isolation). It is necessary but not sufficient for cutover — boot-dir
// content + the headless smoke are the other half (see
// plant_smoke_test.go and the S5 prompt amendment).
package launchparity

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/hollis-labs/agentkit/agentlaunch/parity"
)

// specsRoot is the Tether launch corpus authored in S5 prep-B, relative
// to this package directory.
const specsRoot = "../../testdata/launch-specs"

// minimumConfigBag is the synthetic smallest-legal-bag fixture; it has no
// legacy counterpart, so there is nothing to diff it against.
const minimumConfigBag = "tether-minimum"

// expectedOldErrors registers the launches whose legacy YAML references
// an agent absent from ~/.tether/catalog/agents/ — the old (catalog-walk)
// side cannot resolve them. Documented data defects, not parity failures:
// the new-side bags fold the dangling agent into the agent_role input and
// resolve fine. Tracked in Vanta (limitations.tether.live_catalog_data_defects).
// hollislabs-web-writer-claude is already in the harness's built-in
// registry; these two surfaced when the corpus widened 11 -> 64.
var expectedOldErrors = map[string]string{
	"hollislabs-web-claude": "dangling-agent: legacy launch references agent:web-engineer " +
		"with no agents/web-engineer.yaml; new bag folds it into agent_role",
	"stack-explorer-auditor-codex": "dangling-agent: legacy launch references " +
		"agent:stack-explorer-auditor with no agents/stack-explorer-auditor.yaml; " +
		"new bag folds it into agent_role",
}

// agentMuxRepointed is the agent-mux launches NOT in the harness's
// built-in expected-diff registry. The built-in registry covers the five
// S4.4-sample agent-mux bags; Tether's full corpus has ten. Every
// agent-mux bag correctly scopes to project agent-mux while the legacy
// launches/agent-mux-*.yaml carry project:tether (cloned, never
// re-pointed — see limitations.tether.live_catalog_data_defects Defect 2),
// so all ten diff old-vs-new on project + work_dir. These five need a
// caller-side registration; the other five the harness already knows.
var agentMuxRepointed = []string{
	"agent-mux-claude-stream",
	"agent-mux-claude-worktree",
	"agent-mux-codex-app-server-worktree",
	"agent-mux-codex-launch-worktree",
	"agent-mux-opencode-worktree",
}

// expectedAgentMuxDiffs builds the project + work_dir ExpectedDiff pair
// for each agent-mux launch in agentMuxRepointed.
func expectedAgentMuxDiffs() []parity.ExpectedDiff {
	const rationale = "agent-mux-project-repoint: legacy launch carries project:tether " +
		"(cloned, never re-pointed); the S4.4/S5 bag correctly scopes to agent-mux"
	var diffs []parity.ExpectedDiff
	for _, launch := range agentMuxRepointed {
		diffs = append(diffs,
			parity.ExpectedDiff{
				Launch: launch, Field: "project",
				Old: "tether", New: "agent-mux", Rationale: rationale,
			},
			parity.ExpectedDiff{
				Launch: launch, Field: "work_dir",
				Old: "~/dev/hollis-labs/apps/tether", New: "~/dev/hollis-labs/apps/agent-mux",
				Rationale: rationale,
			},
		)
	}
	return diffs
}

// fullCorpus builds the parity corpus from every bag file on disk. Each
// bag was authored with its filename stem equal to the legacy launch id
// it re-expresses (S5 prep-B), so the mapping is 1:1.
func fullCorpus(t *testing.T) []parity.CorpusEntry {
	t.Helper()
	bags, err := filepath.Glob(filepath.Join(specsRoot, "launches", "*.yaml"))
	if err != nil {
		t.Fatalf("glob launch bags: %v", err)
	}
	if len(bags) == 0 {
		t.Fatalf("no launch bags found under %s/launches", specsRoot)
	}
	var corpus []parity.CorpusEntry
	for _, bag := range bags {
		stem := strings.TrimSuffix(filepath.Base(bag), ".yaml")
		if stem == minimumConfigBag {
			continue
		}
		corpus = append(corpus, parity.CorpusEntry{BagFile: stem, LegacyID: stem})
	}
	sort.Slice(corpus, func(i, j int) bool { return corpus[i].BagFile < corpus[j].BagFile })
	return corpus
}

// TestFullCorpusParity runs the S4.5 parity harness over all 64 Tether
// launches. It skips cleanly when the live catalog is absent (e.g. a CI
// runner with no Tether install) — that is not a parity failure.
func TestFullCorpusParity(t *testing.T) {
	catalogRoot := parity.DefaultCatalogRoot()
	if err := parity.RequireCatalog(catalogRoot); err != nil {
		t.Skipf("live catalog absent, skipping parity: %v", err)
	}

	report, err := parity.RunParity(catalogRoot, specsRoot,
		parity.WithCorpus(fullCorpus(t)),
		parity.WithExpectedOldErrors(expectedOldErrors),
		parity.WithExpectedDiffs(expectedAgentMuxDiffs()...),
	)
	if err != nil {
		t.Fatalf("RunParity: %v", err)
	}
	t.Log("\n" + report.Summary())

	for _, u := range report.UnexplainedDiffs() {
		t.Errorf("UNEXPLAINED diff: launch=%s field=%s old=%q new=%q",
			u.Launch, u.Diff.Field, u.Diff.Old, u.Diff.New)
	}
	if !report.Passed() {
		t.Errorf("parity not green over %d-launch corpus — see summary above", len(report.Cases))
	}

	// Rot-guard: every registered expected divergence must have been
	// observed. A stale entry means a defect was fixed (or a launch
	// removed) and the registration should be dropped.
	if stale := report.StaleExpected(); len(stale) > 0 {
		t.Errorf("stale expected-divergence registrations (no longer observed): %v", stale)
	}
}
