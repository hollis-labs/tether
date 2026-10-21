package eventingest

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents"
	"github.com/hollis-labs/libs/security/redact"
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
		s, isAllow, _ := p.forKind(kind)
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
	s, isAllow, _ := p.forKind("task.state_changed")
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
	s, isAllow, _ := p.forKind("data.emitted")
	if isAllow {
		t.Fatal("data.emitted is free-form and must use the denylist")
	}
	out, _ := s.Sanitize([]byte(`{"schema":"x@1","data":{"custom":"keep","password":"hunter2hunter2"}}`))
	if !strings.Contains(string(out), "keep") || strings.Contains(string(out), "hunter2hunter2") {
		t.Fatalf("%s", out)
	}
}

// A schema that is declared but broken is a broken policy, not a free-form one:
// it must stop the load, name the kind and the schema, and never become the
// denylist.
func TestBrokenPolicyIsRefusedAtLoadNotDegraded(t *testing.T) {
	old := payloadSchemaFor
	t.Cleanup(func() { payloadSchemaFor = old })
	cases := map[string]func(string) ([]byte, error){
		"schema unreadable": func(string) ([]byte, error) { return nil, errors.New("embedded schema missing") },
		"schema not JSON":   func(string) ([]byte, error) { return []byte(`{"properties":`), nil },
	}
	for name, src := range cases {
		payloadSchemaFor = src
		p, err := newPolicies()
		if err == nil || p != nil {
			t.Fatalf("%s: newPolicies = %v, %v; want a refusal", name, p, err)
		}
		if !strings.Contains(err.Error(), `payload schema "urn:hollis-labs:events:`) || !strings.Contains(err.Error(), ` for kind "`) {
			t.Errorf("%s: error names neither kind nor schema: %v", name, err)
		}
		if _, err := New(nil, nil, Config{}); err == nil {
			t.Errorf("%s: New enabled ingest on a broken policy", name)
		}
	}
}

func TestAllowlistThatDoesNotCompileIsRefusedNotDegraded(t *testing.T) {
	old := newAllowlist
	t.Cleanup(func() { newAllowlist = old })
	newAllowlist = func([]string) (*redact.Sanitizer, error) { return nil, errors.New("bad path") }
	if _, err := newPolicies(); err == nil || !strings.Contains(err.Error(), "does not compile") {
		t.Fatalf("newPolicies error = %v", err)
	}
	// forKind after the fact reports the error rather than returning the denylist.
	p := &policies{kind: map[string]*redact.Sanitizer{}}
	if s, _, err := p.forKind("task.state_changed"); err == nil || s != nil {
		t.Fatalf("forKind = %v, %v; want an error and no sanitizer", s, err)
	}
}

// If a kind's policy is unavailable when an event arrives, its payload is not
// stored (strict), not scrubbed by a looser policy.
func TestUnavailablePolicyStoresNoPayload(t *testing.T) {
	old := payloadSchemaFor
	t.Cleanup(func() { payloadSchemaFor = old })
	payloadSchemaFor = func(string) ([]byte, error) { return nil, errors.New("gone") }
	svc := &Service{policies: &policies{kind: map[string]*redact.Sanitizer{}}}
	ev := runtimeevents.Event{Kind: "task.state_changed", Payload: json.RawMessage(`{"entity":{"type":"task","id":"T-1"},"smuggled":"x"}`)}
	out, class := svc.sanitize(ev)
	if out != `{"sanitize_error":"policy_unavailable"}` || class.Sanitized {
		t.Fatalf("sanitize = %q sanitized=%v", out, class.Sanitized)
	}
	if strings.Contains(out, "smuggled") || strings.Contains(out, "T-1") {
		t.Fatal("payload content stored under a broken policy")
	}
}

// The accepted DEV fallback: no schema, free-form, or composed schemas keep the
// denylist (secrets and PII still scrubbed).
func TestSchemalessAndFreeformKindsStillUseTheDenylist(t *testing.T) {
	p, err := newPolicies()
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"data.emitted", "no.such.kind"} {
		s, isAllow, err := p.forKind(kind)
		if err != nil || s == nil || isAllow {
			t.Errorf("%s: %v allow=%v err=%v; want the denylist", kind, s, isAllow, err)
		}
	}
}
