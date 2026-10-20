package eventingest

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAllowedPathsFromCatalogSchemas(t *testing.T) {
	paths, ok := allowedPaths("task.state_changed")
	if !ok {
		t.Fatal("task.state_changed should reduce to a path list")
	}
	joined := strings.Join(paths, " ")
	for _, want := range []string{"/entity/type", "/entity/id", "/from", "/to"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %s in %v", want, paths)
		}
	}
	// Free-form or composed schemas cannot become an allowlist.
	for _, kind := range []string{"data.emitted", "no.such.kind"} {
		if _, ok := allowedPaths(kind); ok {
			t.Errorf("%s must fall back to the denylist", kind)
		}
	}
}

func TestEveryCatalogKindHasASanitizer(t *testing.T) {
	p, err := newPolicies()
	if err != nil {
		t.Fatal(err)
	}
	var allow, deny int
	for _, kind := range []string{"task.state_changed", "run.state_changed", "alert.raised", "approval.requested", "audit.recorded", "data.emitted", "fleet.node.down", "message.sent", "process.started"} {
		s, isAllow := p.forKind(kind)
		if s == nil {
			t.Fatalf("%s has no sanitizer", kind)
		}
		if isAllow {
			allow++
		} else {
			deny++
		}
	}
	if allow == 0 || deny == 0 {
		t.Errorf("expected both policy kinds in use, allow=%d deny=%d", allow, deny)
	}
}

func TestAllowlistDropsUndeclaredFields(t *testing.T) {
	p, _ := newPolicies()
	s, isAllow := p.forKind("task.state_changed")
	if !isAllow {
		t.Skip("task.state_changed is not an allowlist kind in this catalog version")
	}
	in := `{"entity":{"type":"task","id":"T-1"},"from":"todo","to":"doing","trigger":{"kind":"agent"},"smuggled":"note=hello","nested":{"deep":"x"}}`
	out, rep := s.Sanitize([]byte(in))
	if rep.ErrorClass != "" {
		t.Fatalf("error class %q", rep.ErrorClass)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if _, has := got["smuggled"]; has {
		t.Errorf("undeclared field kept: %s", out)
	}
	if _, has := got["nested"]; has {
		t.Errorf("undeclared object kept: %s", out)
	}
	if got["to"] != "doing" {
		t.Errorf("declared field lost: %s", out)
	}
	if rep.Redactions == 0 {
		t.Errorf("removals not counted: %+v", rep)
	}
}

func TestFreeFormKindKeepsShapeButScrubsSecrets(t *testing.T) {
	p, _ := newPolicies()
	s, isAllow := p.forKind("data.emitted")
	if isAllow {
		t.Fatal("data.emitted is free-form and must use the denylist")
	}
	out, _ := s.Sanitize([]byte(`{"schema":"x@1","data":{"custom":"keep","password":"hunter2hunter2"}}`))
	if !strings.Contains(string(out), "keep") || strings.Contains(string(out), "hunter2hunter2") {
		t.Fatalf("%s", out)
	}
}
