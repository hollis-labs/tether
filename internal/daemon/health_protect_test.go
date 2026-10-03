package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// /health carries the daemon's own view of control-plane protection when the
// server has one, and omits the field otherwise, so `tether doctor` can tell a
// daemon that reports it from one that does not (CW-20261001-0142).
func TestHandleHealth_SandboxProtect(t *testing.T) {
	get := func(s *Server) (Health, string) {
		rr := httptest.NewRecorder()
		s.handleHealth(rr, httptest.NewRequest(http.MethodGet, "/health", nil))
		var h Health
		if err := json.Unmarshal(rr.Body.Bytes(), &h); err != nil {
			t.Fatalf("decode %q: %v", rr.Body.String(), err)
		}
		return h, rr.Body.String()
	}

	h, raw := get(&Server{})
	if h.SandboxProtect != nil || strings.Contains(raw, "sandbox_protect") {
		t.Fatalf("a server with no protection view reported one: %s", raw)
	}

	want := SandboxProtectHealth{Enabled: true, Reason: "on", Codex: "not protected", CodexReason: "not protected (CW-20261001-0230): codex runs as before", BwrapChecked: true, BwrapError: "uid map: Permission denied"}
	h, raw = get(&Server{SandboxProtect: func() *SandboxProtectHealth { return &want }})
	if h.SandboxProtect == nil || !reflect.DeepEqual(*h.SandboxProtect, want) {
		t.Fatalf("sandbox_protect = %+v, want %+v (%s)", h.SandboxProtect, want, raw)
	}
	for _, field := range []string{`"enabled":true`, `"bwrap_checked":true`, `"bwrap_error":"uid map: Permission denied"`, `"codex":"not protected"`, `"codex_reason":"not protected (CW-20261001-0230): codex runs as before"`} {
		if !strings.Contains(raw, field) {
			t.Errorf("/health body lacks %s: %s", field, raw)
		}
	}
	// With nothing skipped and no plan failure, neither field is in the body.
	for _, absent := range []string{"skipped_project_layers", "created_project_roots", "anchored_project_ancestors", "plan_error"} {
		if strings.Contains(raw, absent) {
			t.Errorf("/health body carries %s with nothing to report: %s", absent, raw)
		}
	}
}

// The skipped project layers and a plan failure travel as their own fields, apart
// from the bubblewrap probe (CW-20261003-0092).
func TestHandleHealth_SandboxProtectSkippedLayersAndPlanError(t *testing.T) {
	want := SandboxProtectHealth{Enabled: true, Reason: "on", BwrapChecked: true, BwrapUsable: true,
		SkippedProjectLayers:     []SkippedProjectLayer{{Project: "old-site", RepoRoot: "/home/u/old-site", Reason: "does not exist"}},
		CreatedProjectRoots:      []CreatedProjectRoot{{Project: "gone", RepoRoot: "/home/u/gone", Reason: "created"}},
		AnchoredProjectAncestors: []AnchoredProjectAncestor{{Project: "nas", RepoRoot: "/home/u/mnt/nas/repo", Ancestor: "/home/u/mnt/nas", Reason: "anchored"}},
		PlanError:                "protect catalog layer: project \"x\": boom"}
	rr := httptest.NewRecorder()
	(&Server{SandboxProtect: func() *SandboxProtectHealth { return &want }}).handleHealth(rr, httptest.NewRequest(http.MethodGet, "/health", nil))
	var h Health
	if err := json.Unmarshal(rr.Body.Bytes(), &h); err != nil {
		t.Fatalf("decode %q: %v", rr.Body.String(), err)
	}
	if h.SandboxProtect == nil || !reflect.DeepEqual(*h.SandboxProtect, want) {
		t.Fatalf("sandbox_protect = %+v, want %+v (%s)", h.SandboxProtect, want, rr.Body.String())
	}
	for _, field := range []string{`"skipped_project_layers":[{"project":"old-site","repo_root":"/home/u/old-site","reason":"does not exist"}]`, `"created_project_roots":[{"project":"gone","repo_root":"/home/u/gone","reason":"created"}]`, `"anchored_project_ancestors":[{"project":"nas","repo_root":"/home/u/mnt/nas/repo","ancestor":"/home/u/mnt/nas","reason":"anchored"}]`, `"plan_error":"protect catalog layer: project \"x\": boom"`, `"bwrap_usable":true`} {
		if !strings.Contains(rr.Body.String(), field) {
			t.Errorf("/health body lacks %s: %s", field, rr.Body.String())
		}
	}
}
