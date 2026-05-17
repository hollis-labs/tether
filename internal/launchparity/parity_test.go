// Package launchparity drives the go-agent-launch S4.5 parity harness
// over Tether's FULL launch corpus.
//
// The harness's own parity.RunParity walks a hardcoded 11-entry
// parity.Corpus — the S4.4 representative sample. Tether's cutover gate
// (EP-20260516-0001 S5.1) needs parity proven over all 64 legacy
// launches, so this test rebuilds parity.Corpus from every bag in
// testdata/launch-specs/launches/ (bag stem == legacy launch id, 1:1)
// and re-runs the harness.
//
// Parity proves launch *identity* (project / work_dir / runner /
// isolation). It is necessary but not sufficient for cutover — boot-dir
// content + the headless smoke are the other half (see the S5 prompt
// amendment). A green run here is the identity half of the gate.
//
// # Two documented harness-gap compensations
//
// The S4.5 parity harness compares raw field values. Two classes of diff
// it reports are NOT real divergences; both are tracked for the
// go-agent-launch parity-harness-extensibility follow-up, and this driver
// compensates for them locally so the Tether identity gate is meaningful
// today:
//
//  1. work_dir tilde expansion — the old (catalog) side keeps "~/dev/x";
//     the new (Spec) side resolves it to an absolute path. Same directory.
//     normalizeWorkDir expands "~" on both sides before comparing.
//
//  2. dangling-agent data defects — hollislabs-web-claude and
//     stack-explorer-auditor-codex reference agents absent from the live
//     catalog, so the old side errors. Same defect class as the
//     harness-registered web-writer case. Tracked in Vanta
//     (limitations.tether.live_catalog_data_defects); allow-listed here
//     until expected_diffs.go carries them.
package launchparity

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/hollis-labs/go-agent-launch/agentlaunch/parity"
)

// specsRoot is the Tether launch corpus authored in S5 prep-B, relative
// to this package directory.
const specsRoot = "../../testdata/launch-specs"

// minimumConfigBag is the synthetic smallest-legal-bag fixture; it has no
// legacy counterpart, so there is nothing to diff it against.
const minimumConfigBag = "tether-minimum"

// danglingAgentDefects are launches whose legacy YAML references an agent
// absent from ~/.tether/catalog/agents/ — the old side cannot resolve
// them. Documented data defects, not parity failures (see package doc and
// Vanta limitations.tether.live_catalog_data_defects). The new-side bags
// fold the dangling agent into the agent_role input and resolve fine.
var danglingAgentDefects = map[string]bool{
	"hollislabs-web-claude":        true,
	"stack-explorer-auditor-codex": true,
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

// normalizeWorkDir expands a leading "~/" to the user's home directory so
// the catalog side ("~/dev/x") and the Spec side ("/Users/.../dev/x")
// compare equal. Harness-gap compensation #1 (see package doc).
func normalizeWorkDir(t *testing.T, p string) string {
	t.Helper()
	if !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, p[2:])
}

// TestFullCorpusParity runs the S4.5 parity harness over all 64 Tether
// launches. It skips cleanly when the live catalog is absent (e.g. a CI
// runner with no Tether install) — that is not a parity failure.
func TestFullCorpusParity(t *testing.T) {
	catalogRoot := parity.DefaultCatalogRoot()
	if err := parity.RequireCatalog(catalogRoot); err != nil {
		t.Skipf("live catalog absent, skipping parity: %v", err)
	}

	// parity.Corpus is a package-level var the harness iterates; widen
	// it from the S4.4 sample to Tether's full corpus for this run.
	parity.Corpus = fullCorpus(t)

	report, err := parity.RunParity(catalogRoot, specsRoot)
	if err != nil {
		t.Fatalf("RunParity: %v", err)
	}
	t.Log("\n" + report.Summary())

	identical, normalized, dataDefect, explained, failed := 0, 0, 0, 0, 0
	for _, c := range report.Cases {
		// Old-side resolve errors: a documented dangling-agent data
		// defect is expected; anything else is a real failure.
		if c.OldErr != nil {
			if danglingAgentDefects[c.Launch] {
				dataDefect++
				continue
			}
			// The harness itself may already explain it (web-writer).
			if c.Parity() {
				explained++
				continue
			}
			t.Errorf("unexpected old-side resolve error: launch=%s err=%v", c.Launch, c.OldErr)
			failed++
			continue
		}
		if c.NewErr != nil {
			t.Errorf("new-side resolve error: launch=%s err=%v", c.Launch, c.NewErr)
			failed++
			continue
		}

		var real []parity.FieldDiff
		for _, d := range c.Diffs {
			switch {
			case d.Explained():
				// harness-registered expected divergence
			case d.Field == "work_dir" &&
				normalizeWorkDir(t, d.Old) == normalizeWorkDir(t, d.New):
				// harness-gap compensation #1: tilde expansion
			default:
				real = append(real, d)
			}
		}
		switch {
		case len(real) > 0:
			for _, d := range real {
				t.Errorf("UNEXPLAINED diff: launch=%s field=%s old=%q new=%q",
					c.Launch, d.Field, d.Old, d.New)
			}
			failed++
		case len(c.Diffs) == 0:
			identical++
		default:
			normalized++
		}
	}

	t.Logf("parity over %d launches: %d identical, %d normalized-equal, %d explained, %d documented-data-defect, %d failed",
		len(report.Cases), identical, normalized, explained, dataDefect, failed)
	if failed > 0 {
		t.Errorf("parity not green: %d launch(es) diverge — see above", failed)
	}
}
