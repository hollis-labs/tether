package registry

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func neverCollides(_ context.Context, _ string) (bool, error)  { return false, nil }
func alwaysCollides(_ context.Context, _ string) (bool, error) { return true, nil }

func TestMintAgentURN_format(t *testing.T) {
	// Deterministic source: 10 zero bytes → alphabet[0] × 10 = "aaaaaaaaaa".
	src := bytes.NewReader(make([]byte, idSuffixLen))
	urn, err := mintURN(context.Background(), src, urnKindAgent, defaultAuthority, agentIDPrefix, neverCollides)
	if err != nil {
		t.Fatalf("mintURN: %v", err)
	}
	const want = "msg://agent/agent-mux/agt_aaaaaaaaaa"
	if urn != want {
		t.Fatalf("urn = %q; want %q", urn, want)
	}
}

func TestMintProjectURN_format(t *testing.T) {
	src := bytes.NewReader(make([]byte, idSuffixLen))
	urn, err := mintURN(context.Background(), src, urnKindAgent, defaultAuthority, projectIDPrefix, neverCollides)
	if err != nil {
		t.Fatalf("mintURN: %v", err)
	}
	if !strings.HasPrefix(urn, "msg://agent/agent-mux/prj_") {
		t.Fatalf("urn = %q; want prj_ prefix", urn)
	}
	wantLen := len("msg://agent/agent-mux/prj_") + idSuffixLen
	if len(urn) != wantLen {
		t.Fatalf("urn length = %d; want %d", len(urn), wantLen)
	}
}

// TestMintGroupURN_format verifies the 3-segment group URN shape with
// the URN-kind segment switched from `agent` to `group`. Default
// authority is `agent-mux` to match v060-01 deployments.
func TestMintGroupURN_format(t *testing.T) {
	src := bytes.NewReader(make([]byte, idSuffixLen))
	urn, err := mintURN(context.Background(), src, urnKindGroup, defaultAuthority, groupIDPrefix, neverCollides)
	if err != nil {
		t.Fatalf("mintURN: %v", err)
	}
	const want = "msg://group/agent-mux/grp_aaaaaaaaaa"
	if urn != want {
		t.Fatalf("urn = %q; want %q", urn, want)
	}
}

// TestMintGroupURN_publicEntryPoint exercises the exported MintGroupURN
// against neverCollides, verifying the authority parameter is honored
// (and falls back to default when empty).
func TestMintGroupURN_publicEntryPoint(t *testing.T) {
	tests := []struct {
		name      string
		authority string
		wantPath  string
	}{
		{name: "default authority on empty", authority: "", wantPath: "msg://group/agent-mux/grp_"},
		{name: "explicit authority", authority: "tether-east", wantPath: "msg://group/tether-east/grp_"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			urn, err := MintGroupURN(context.Background(), tc.authority, neverCollides)
			if err != nil {
				t.Fatalf("MintGroupURN: %v", err)
			}
			if !strings.HasPrefix(urn, tc.wantPath) {
				t.Fatalf("urn = %q; want prefix %q", urn, tc.wantPath)
			}
			wantLen := len(tc.wantPath) + idSuffixLen
			if len(urn) != wantLen {
				t.Fatalf("urn length = %d; want %d", len(urn), wantLen)
			}
		})
	}
}

func TestRandSuffix_alphabetOnly(t *testing.T) {
	// Feed every possible byte value through the sampler; the resulting
	// suffix must consist only of idAlphabet characters.
	bs := make([]byte, 256)
	for i := range bs {
		bs[i] = byte(i)
	}
	suffix, err := randSuffix(bytes.NewReader(bs), idSuffixLen)
	if err != nil {
		t.Fatalf("randSuffix: %v", err)
	}
	if len(suffix) != idSuffixLen {
		t.Fatalf("suffix length = %d; want %d", len(suffix), idSuffixLen)
	}
	for _, c := range suffix {
		if !strings.ContainsRune(idAlphabet, c) {
			t.Fatalf("suffix %q contains non-alphabet rune %q", suffix, c)
		}
	}
}

func TestMintURN_collisionRetry(t *testing.T) {
	// Feed 20 zero bytes — two minter attempts with identical suffix.
	// exists returns true on the first call (collide), false on the
	// second (success), proving the minter retries past a collision.
	src := bytes.NewReader(make([]byte, idSuffixLen*2))
	calls := 0
	exists := func(_ context.Context, _ string) (bool, error) {
		calls++
		return calls == 1, nil
	}
	urn, err := mintURN(context.Background(), src, urnKindAgent, defaultAuthority, agentIDPrefix, exists)
	if err != nil {
		t.Fatalf("mintURN: %v", err)
	}
	if calls != 2 {
		t.Fatalf("exists called %d times; want 2 (one collision + one success)", calls)
	}
	const want = "msg://agent/agent-mux/agt_aaaaaaaaaa"
	if urn != want {
		t.Fatalf("urn = %q; want %q", urn, want)
	}
}

func TestMintURN_exhausted(t *testing.T) {
	src := bytes.NewReader(make([]byte, idSuffixLen*mintMaxRetries))
	_, err := mintURN(context.Background(), src, urnKindAgent, defaultAuthority, agentIDPrefix, alwaysCollides)
	if !errors.Is(err, ErrMintExhausted) {
		t.Fatalf("err = %v; want ErrMintExhausted", err)
	}
}

func TestMintURN_contextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	src := bytes.NewReader(make([]byte, idSuffixLen))
	_, err := mintURN(ctx, src, urnKindAgent, defaultAuthority, agentIDPrefix, neverCollides)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v; want context.Canceled", err)
	}
}

func TestMintURN_exhaustedAtBoundary(t *testing.T) {
	// Same as Exhausted but verifies the exact retry count: exists is
	// called mintMaxRetries times when every attempt collides.
	src := bytes.NewReader(make([]byte, idSuffixLen*mintMaxRetries))
	calls := 0
	collides := func(_ context.Context, _ string) (bool, error) {
		calls++
		return true, nil
	}
	_, err := mintURN(context.Background(), src, urnKindAgent, defaultAuthority, agentIDPrefix, collides)
	if !errors.Is(err, ErrMintExhausted) {
		t.Fatalf("err = %v; want ErrMintExhausted", err)
	}
	if calls != mintMaxRetries {
		t.Fatalf("exists calls = %d; want %d", calls, mintMaxRetries)
	}
}

// TestParseRegistryURN covers the v060-05 group URN structural amendment.
// Accepts both the agent path (msg://agent/<authority>/<id> with agt_/prj_)
// and the group path (msg://group/<authority>/<grp_id>). Rejects mismatched
// id-prefix-vs-urn-kind, missing scheme, wrong segment count, empty
// segments, and unknown URN kinds.
func TestParseRegistryURN(t *testing.T) {
	tests := []struct {
		name      string
		urn       string
		wantKind  string
		wantAuth  string
		wantID    string
		wantErr   bool
		errSubstr string
	}{
		{name: "agent URN", urn: "msg://agent/agent-mux/agt_abc1234567", wantKind: "agent", wantAuth: "agent-mux", wantID: "agt_abc1234567"},
		{name: "project URN", urn: "msg://agent/agent-mux/prj_xyz1234567", wantKind: "agent", wantAuth: "agent-mux", wantID: "prj_xyz1234567"},
		{name: "group URN", urn: "msg://group/agent-mux/grp_q1w2e3r4t5", wantKind: "group", wantAuth: "agent-mux", wantID: "grp_q1w2e3r4t5"},
		{name: "group URN non-default authority", urn: "msg://group/tether-east/grp_q1w2e3r4t5", wantKind: "group", wantAuth: "tether-east", wantID: "grp_q1w2e3r4t5"},
		{name: "missing scheme", urn: "agent/agent-mux/agt_abc1234567", wantErr: true, errSubstr: "missing scheme"},
		{name: "too few segments", urn: "msg://group/grp_q1w2e3r4t5", wantErr: true, errSubstr: "want 3 path segments"},
		{name: "too many segments", urn: "msg://agent/agent-mux/agt_x/sub", wantErr: true, errSubstr: "want 3 path segments"},
		{name: "empty authority", urn: "msg://agent//agt_abc1234567", wantErr: true, errSubstr: "empty authority"},
		{name: "empty id", urn: "msg://agent/agent-mux/", wantErr: true, errSubstr: "empty authority or id"},
		{name: "unknown urn kind", urn: "msg://service/agent-mux/svc_abc1234567", wantErr: true, errSubstr: "unknown URN kind"},
		{name: "agent path with grp_ id", urn: "msg://agent/agent-mux/grp_q1w2e3r4t5", wantErr: true, errSubstr: "agent URN id must start"},
		{name: "group path with agt_ id", urn: "msg://group/agent-mux/agt_abc1234567", wantErr: true, errSubstr: "group URN id must start"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseRegistryURN(tc.urn)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseRegistryURN(%q): err = nil; want substring %q", tc.urn, tc.errSubstr)
				}
				if !errors.Is(err, ErrInvalidURN) {
					t.Fatalf("ParseRegistryURN(%q): err = %v; want errors.Is(err, ErrInvalidURN)", tc.urn, err)
				}
				if !strings.Contains(err.Error(), tc.errSubstr) {
					t.Fatalf("ParseRegistryURN(%q): err = %v; want substring %q", tc.urn, err, tc.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRegistryURN(%q): %v", tc.urn, err)
			}
			if got.URNKind != tc.wantKind {
				t.Errorf("URNKind = %q; want %q", got.URNKind, tc.wantKind)
			}
			if got.Authority != tc.wantAuth {
				t.Errorf("Authority = %q; want %q", got.Authority, tc.wantAuth)
			}
			if got.ID != tc.wantID {
				t.Errorf("ID = %q; want %q", got.ID, tc.wantID)
			}
		})
	}
}

// TestIsGroupURN covers the convenience predicate used by routing layers
// that only need the dispatch bit.
func TestIsGroupURN(t *testing.T) {
	cases := map[string]bool{
		"msg://group/agent-mux/grp_q1w2e3r4t5":   true,
		"msg://group/tether-east/grp_aaaaaaaaaa": true,
		"msg://agent/agent-mux/agt_abc1234567":   false,
		"msg://agent/agent-mux/prj_xyz1234567":   false,
		"msg://group/agent-mux/agt_abc1234567":   false, // invalid (id mismatch) → not a group URN
		"":                                       false,
		"garbage":                                false,
	}
	for urn, want := range cases {
		if got := IsGroupURN(urn); got != want {
			t.Errorf("IsGroupURN(%q) = %v; want %v", urn, got, want)
		}
	}
}
