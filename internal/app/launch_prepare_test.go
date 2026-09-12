package app

import (
	"slices"
	"testing"

	"github.com/hollis-labs/tether/internal/store"
)

// TestMuxMCPPlant_ArgvAndAttributionCannotDisagree is the whole reason this
// function returns a struct instead of a []string.
//
// The failure it prevents is the recurring one this sprint kept finding: a
// component correct in isolation, wired differently by the composition root. An
// attribution computed anywhere other than here would be a second statement of
// one decision, and the two would drift the first time the flag changed. The
// test asserts the pairing directly -- for each planting, the flags actually
// emitted and the attribution reported must agree.
func TestMuxMCPPlant_ArgvAndAttributionCannotDisagree(t *testing.T) {
	cases := []struct {
		name            string
		sessionID       string
		extractRefs     bool
		wantSession     bool
		wantExtract     bool
		wantAttribution string
	}{
		{
			name: "no session id means nothing can be attributed",
			// `mux boot` execs into the native CLI and creates no session row.
			sessionID: "", extractRefs: false,
			wantSession: false, wantExtract: false,
			wantAttribution: store.RefAttributionUnlaunched,
		},
		{
			name: "launched but extraction off is the state of every session today",
			// CW-20260912-0112: there is no config seam that can turn it on.
			sessionID: "sess-1", extractRefs: false,
			wantSession: true, wantExtract: false,
			wantAttribution: store.RefAttributionNone,
		},
		{
			name:      "extraction on is the only state that can produce a proxy ref",
			sessionID: "sess-1", extractRefs: true,
			wantSession: true, wantExtract: true,
			wantAttribution: store.RefAttributionProxy,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := MuxMCPPlant("/catalog", tc.sessionID, tc.extractRefs)

			if got := slices.Contains(plan.Args, "--session"); got != tc.wantSession {
				t.Errorf("--session present = %v, want %v (argv: %v)", got, tc.wantSession, plan.Args)
			}
			if got := slices.Contains(plan.Args, "--extract-refs"); got != tc.wantExtract {
				t.Errorf("--extract-refs present = %v, want %v (argv: %v)", got, tc.wantExtract, plan.Args)
			}
			if plan.Attribution != tc.wantAttribution {
				t.Errorf("attribution = %q, want %q", plan.Attribution, tc.wantAttribution)
			}

			// The coupling itself, stated as an invariant rather than as three
			// separate expectations: only a planting that carries BOTH flags
			// may claim it can produce a proxy-observed ref.
			canProduce := store.CanProduceProxyRefs(plan.Attribution)
			bothFlags := slices.Contains(plan.Args, "--session") && slices.Contains(plan.Args, "--extract-refs")
			if canProduce != bothFlags {
				t.Errorf("attribution %q claims can-produce=%v but the argv carries both flags=%v; the stamp and the planting have diverged",
					plan.Attribution, canProduce, bothFlags)
			}
		})
	}
}

// TestMuxMCPPlant_AlwaysCarriesTheProxyContract guards the parts every planting
// needs regardless of attribution -- a worker without them is not sandboxed,
// it is broken.
func TestMuxMCPPlant_AlwaysCarriesTheProxyContract(t *testing.T) {
	for _, sessionID := range []string{"", "sess-1"} {
		args := MuxMCPPlant("/catalog", sessionID, false).Args
		for _, want := range []string{"--catalog", "/catalog", "mcp", "--proxy", "--token", "--scopes"} {
			if !slices.Contains(args, want) {
				t.Errorf("sessionID=%q: argv missing %q: %v", sessionID, want, args)
			}
		}
	}
}
