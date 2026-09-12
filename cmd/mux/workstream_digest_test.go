package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/api"
)

// digestFixture is the constructed lineage the docs' worked example renders.
//
// CONSTRUCTED, AND LABELED AS SUCH WHEREVER IT APPEARS. Measured 2026-09-12
// against the live DB: 129 sessions, all intent 'fresh', none with a parent,
// one workstream containing zero sessions. There is no production workstream
// that spans a compaction, so a "real" example is not available and will not be
// until sessions start recording lineage. Hand-authoring plausible JSON that
// looks like production output would be fabricated evidence; generating the
// sample from the renderer under test is not.
func digestFixture() api.DigestResponse {
	return api.DigestResponse{
		Grain: "workstream",
		Workstream: &api.WorkstreamDTO{
			ID: "01a09759-f4fa-7a6f-8738-d702b1330fa5", Name: "session-correlation",
			WorkflowID: "SP-20260912-0001", Status: "active",
			CreatedAt: "2026-09-12T15:00:00Z", UpdatedAt: "2026-09-12T21:00:00Z",
		},
		Span: api.DigestSpanDTO{
			SessionCount: 3, SpansLineage: true,
			Sessions: []api.DigestSessionDTO{
				{ID: "sess-a", Intent: "fresh", State: "completed", CreatedAt: "2026-09-12T15:00:00Z", RefAttribution: "none", RefCount: 2},
				{ID: "sess-b", Intent: "compact", ParentSessionID: "sess-a", State: "completed", CreatedAt: "2026-09-12T18:00:00Z", RefAttribution: "none", RefCount: 2},
				{ID: "sess-c", Intent: "resume", ParentSessionID: "sess-b", State: "running", CreatedAt: "2026-09-12T20:00:00Z", RefAttribution: "none", RefCount: 1},
			},
		},
		LeftBehind: []api.DigestKindGroupDTO{
			{Kind: "torque_task", Created: []api.DigestRefDTO{
				{SessionID: "sess-a", RefID: "CW-20260912-0112", Relation: "created", Source: "agent", At: "2026-09-12T21:18:27Z"},
			}, Updated: []api.DigestRefDTO{
				{SessionID: "sess-c", RefID: "CW-20260912-0063", Relation: "updated", Source: "agent", At: "2026-09-12T21:19:05Z"},
			}},
			{Kind: "git_commit", Created: []api.DigestRefDTO{
				{SessionID: "sess-b", RefID: "cc817eb", Relation: "created", Source: "api", At: "2026-09-12T20:44:00Z"},
			}},
		},
		Touched: []api.DigestKindGroupDTO{
			{Kind: "torque_task", Read: []api.DigestRefDTO{
				{SessionID: "sess-a", RefID: "CW-20260912-0061", Relation: "read", Source: "agent", At: "2026-09-12T16:00:00Z"},
				{SessionID: "sess-b", RefID: "CW-20260912-0060", Relation: "read", Source: "agent", At: "2026-09-12T19:00:00Z"},
			}},
		},
		Totals: api.DigestTotalsDTO{
			Refs: 5, ByRelation: map[string]int{"created": 2, "updated": 1, "read": 2},
			BySource: map[string]int{"agent": 4, "api": 1},
		},
		Coverage: api.DigestCoverageDTO{
			Limit: 500, Truncated: false,
			Attribution: map[string]int{"none": 3}, ProxyAttributable: 0,
			Note: "no session here could produce a proxy-observed ref, because ref extraction was not enabled for it (CW-20260912-0112): an empty proxy column says nothing about what these sessions did",
		},
	}
}

// TestRenderDigest_MatchesTheDocumentedExample keeps docs/workstreams.md
// honest: the sample in the docs is this renderer's real output, and this test
// fails when they diverge.
//
// Regenerate with: UPDATE_DIGEST_GOLDEN=1 go test ./cmd/mux -run RenderDigest
func TestRenderDigest_MatchesTheDocumentedExample(t *testing.T) {
	var buf bytes.Buffer
	renderDigest(&buf, digestFixture(), false)
	got := buf.String()

	const golden = "testdata/digest_example.txt"
	if os.Getenv("UPDATE_DIGEST_GOLDEN") != "" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Skip("regenerated " + golden)
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate with UPDATE_DIGEST_GOLDEN=1)", golden, err)
	}
	if got != string(want) {
		t.Fatalf("render drifted from %s\n--- got ---\n%s\n--- want ---\n%s", golden, got, want)
	}
}

// TestRenderDigest_AlwaysQualifiesAnEmptyProxyColumn.
//
// The renderer is the last place this can be lost, and losing it is silent: a
// digest with no proxy refs and no note reads as "the agent asserted
// everything" rather than "observation was never switched on."
func TestRenderDigest_AlwaysQualifiesAnEmptyProxyColumn(t *testing.T) {
	var buf bytes.Buffer
	renderDigest(&buf, digestFixture(), false)
	out := buf.String()
	if !strings.Contains(out, "CW-20260912-0112") {
		t.Error("the coverage note did not reach the rendered output")
	}
	if !strings.Contains(out, "attribution: none=3") {
		t.Errorf("per-span attribution counts missing from coverage line:\n%s", out)
	}
	// Every session line carries its own attribution too, so a reader scanning
	// the roster never has to cross-reference the summary.
	if strings.Count(out, "attribution=none") != 3 {
		t.Errorf("expected each of the 3 sessions to report its attribution:\n%s", out)
	}
}

// TestRenderDigest_LeftBehindOnlySuppressesTouched pins --left-behind.
func TestRenderDigest_LeftBehindOnlySuppressesTouched(t *testing.T) {
	var full, left bytes.Buffer
	renderDigest(&full, digestFixture(), false)
	renderDigest(&left, digestFixture(), true)
	if !strings.Contains(full.String(), "TOUCHED") {
		t.Fatal("full render is missing the TOUCHED section")
	}
	if strings.Contains(left.String(), "TOUCHED") {
		t.Fatal("--left-behind still rendered the TOUCHED section")
	}
	if !strings.Contains(left.String(), "LEFT BEHIND") {
		t.Fatal("--left-behind dropped the section it exists to show")
	}
}
