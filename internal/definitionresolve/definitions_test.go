package definitionresolve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/substrate/mesh/agentdef"
	"github.com/hollis-labs/tether/internal/fabricstore"
	"github.com/hollis-labs/tether/internal/store"
	"gopkg.in/yaml.v3"
)

const fixtureDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

type fakeContent struct {
	documents map[string][]byte
	badURI    string
	fileSkill bool
	onRead    func()
	reads     int
}

func (f *fakeContent) ReadDefinition(ctx context.Context, source string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.reads++
	if f.onRead != nil {
		f.onRead()
	}
	raw, ok := f.documents[source]
	if !ok {
		return nil, errors.New("source missing")
	}
	return bytes.Clone(raw), nil
}
func (f *fakeContent) Pin(ctx context.Context, uri string) (ContentPin, error) {
	if err := ctx.Err(); err != nil {
		return ContentPin{}, err
	}
	if uri == f.badURI {
		return ContentPin{Kind: FileContent, Digest: "sha256:" + strings.Repeat("2", 64)}, nil
	}
	kind := FileContent
	if uri == "catalog:skill" && !f.fileSkill {
		kind = TreeContent
	}
	return ContentPin{Kind: kind, Digest: fixtureDigest}, nil
}
func minimalDefinition() agentdef.Definition {
	return agentdef.Definition{SchemaVersion: "2", DefinitionID: "example", Revision: "r1", Name: "example", Description: "Example definition", Behavior: agentdef.Behavior{Purpose: "Help with a bounded task"}, HarnessProfile: agentdef.HarnessProfile{Permissions: agentdef.PermissionProfile{Profile: "review"}}, Continuity: agentdef.Continuity{Mode: agentdef.Durable}, Body: "Follow the assignment."}
}
func authored(t *testing.T, d agentdef.Definition) []byte {
	t.Helper()
	raw, err := yaml.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return []byte("---\n" + string(raw) + "---\n" + d.Body + "\n")
}
func definitionHarness(t *testing.T, policy Policy) (*DefinitionStore, *fabricstore.Repository, *fakeContent) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := fabricstore.New(db.DB())
	provider := &fakeContent{documents: map[string][]byte{"catalog:definition": authored(t, minimalDefinition())}}
	if policy.KnownCapability == nil {
		policy.KnownCapability = func(name string) bool { return name == "known" }
	}
	definitions, err := NewDefinitionStore(repo, provider, policy)
	if err != nil {
		t.Fatal(err)
	}
	return definitions, repo, provider
}
func TestIndexAndLoadImmutablePinsAndArtifacts(t *testing.T) {
	definitions, repo, provider := definitionHarness(t, Policy{})
	ctx := t.Context()
	original, err := definitions.Index(ctx, "catalog:definition")
	if err != nil {
		t.Fatal(err)
	}
	retry, err := definitions.Index(ctx, "catalog:definition")
	if err != nil || retry.Pin != original.Pin {
		t.Fatal(retry, err)
	}
	d := minimalDefinition()
	d.Title = "Presentation only"
	provider.documents["catalog:definition"] = authored(t, d)
	if _, err := definitions.Load(ctx, original.Pin); !errors.Is(err, fabricstore.ErrNotFound) {
		t.Fatal("unindexed artifact accepted", err)
	}
	presentation, err := definitions.Index(ctx, "catalog:definition")
	if err != nil || presentation.Pin != original.Pin || presentation.ArtifactDigest == original.ArtifactDigest {
		t.Fatal(presentation, err)
	}
	if _, err := definitions.Load(ctx, original.Pin); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Artifact(ctx, original.Pin, original.ArtifactDigest); err != nil {
		t.Fatal("old artifact was rewritten", err)
	}
	d.Body = "Different semantic instructions."
	provider.documents["catalog:definition"] = authored(t, d)
	if _, err := definitions.Index(ctx, "catalog:definition"); !errors.Is(err, ErrPinMismatch) {
		t.Fatal("revision reassigned", err)
	}
	if _, err := definitions.Load(ctx, original.Pin); !errors.Is(err, ErrPinMismatch) {
		t.Fatal("changed semantic content accepted", err)
	}
	d = minimalDefinition()
	d.DefinitionID = "other-identity"
	provider.documents["catalog:definition"] = authored(t, d)
	if _, err := definitions.Load(ctx, original.Pin); !errors.Is(err, ErrPinMismatch) {
		t.Fatal("semantic hash hid identity change", err)
	}
	delete(provider.documents, "catalog:definition")
	if _, err := definitions.Load(ctx, original.Pin); err == nil {
		t.Fatal("missing original source accepted")
	}
}
func TestStrictSchemaCapabilitiesAndExtensionNegotiation(t *testing.T) {
	definitions, _, provider := definitionHarness(t, Policy{})
	for _, tc := range []struct {
		name   string
		change func(*agentdef.Definition)
	}{
		{"version", func(d *agentdef.Definition) { d.SchemaVersion = "1" }},
		{"capability", func(d *agentdef.Definition) { d.Requirements.Requires = []string{"not-supported"} }},
		{"mandatory-extension", func(d *agentdef.Definition) {
			d.Extensions = map[string]agentdef.Extension{"org.example/content": {Version: "9", Area: "requirements", Mandatory: true, Data: map[string]any{}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := minimalDefinition()
			tc.change(&d)
			provider.documents["catalog:definition"] = authored(t, d)
			if _, err := definitions.Index(t.Context(), "catalog:definition"); err == nil {
				t.Fatal("invalid definition accepted")
			}
		})
	}
	provider.documents["catalog:definition"] = []byte("---\nschema_version: '2'\nunknown_field: yes\n---\nBody\n")
	if _, err := definitions.Index(t.Context(), "catalog:definition"); err == nil {
		t.Fatal("unknown core field accepted")
	}
	d := minimalDefinition()
	d.Extensions = map[string]agentdef.Extension{"org.example/optional": {Version: "9", Area: "behavior", Data: map[string]any{"opaque": "retained"}}}
	provider.documents["catalog:definition"] = authored(t, d)
	got, err := definitions.Index(t.Context(), "catalog:definition")
	if err != nil || got.Definition.Extensions["org.example/optional"].Data["opaque"] != "retained" {
		t.Fatal(got, err)
	}
}

// This test-owned extension has a strict payload schema and meaningful content
// semantics; no always-success negotiation stub is used.
func contentExtension() ExtensionHandler {
	decode := func(extension agentdef.Extension) (agentdef.Ref, error) {
		if extension.Area != "requirements" {
			return agentdef.Ref{}, errors.New("wrong extension area")
		}
		raw, err := json.Marshal(extension.Data)
		if err != nil {
			return agentdef.Ref{}, err
		}
		var payload struct {
			Content agentdef.Ref `json:"content"`
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&payload); err != nil {
			return agentdef.Ref{}, err
		}
		if payload.Content.URI == "" || len(payload.Content.Digest) != 71 || !strings.HasPrefix(payload.Content.Digest, "sha256:") {
			return agentdef.Ref{}, errors.New("extension content pin is incomplete")
		}
		return payload.Content, nil
	}
	return ExtensionHandler{Validate: func(extension agentdef.Extension) error { _, err := decode(extension); return err }, Refs: func(extension agentdef.Extension) ([]agentdef.Ref, error) {
		ref, err := decode(extension)
		return []agentdef.Ref{ref}, err
	}}
}
func TestEveryPinnedReferenceIsVerified(t *testing.T) {
	refs := []string{"instruction", "sop", "input", "output", "skill", "resource", "steering", "context", "context-policy", "approval", "escalation", "memory", "recovery", "extension"}
	ref := func(name string) agentdef.Ref { return agentdef.Ref{URI: "catalog:" + name, Digest: fixtureDigest} }
	d := minimalDefinition()
	d.Behavior.Instructions = []agentdef.Ref{ref("instruction")}
	d.Behavior.SOPs = []agentdef.Ref{ref("sop")}
	input, output, contextPolicy, memory, recovery := ref("input"), ref("output"), ref("context-policy"), ref("memory"), ref("recovery")
	d.Capabilities = []agentdef.Capability{{ID: "service", Description: "Service", Input: &input, Output: &output}}
	d.Requirements.Skills = []agentdef.Skill{{Name: "example-skill", Content: ref("skill")}}
	d.Requirements.Resources = []agentdef.Ref{ref("resource")}
	d.HarnessProfile.Steering = []agentdef.Ref{ref("steering")}
	d.HarnessProfile.Context.Sources = []agentdef.Ref{ref("context")}
	d.HarnessProfile.Context.Policy = &contextPolicy
	d.HarnessProfile.Approvals = []agentdef.Ref{ref("approval")}
	d.HarnessProfile.Escalation = []agentdef.Ref{ref("escalation")}
	d.Continuity.MemoryPolicy = &memory
	d.Continuity.RecoveryStrategy = &recovery
	d.Extensions = map[string]agentdef.Extension{"org.example/content": {Version: "1", Area: "requirements", Mandatory: true, Data: map[string]any{"content": map[string]any{"uri": "catalog:extension", "digest": fixtureDigest}}}}
	policy := Policy{Extensions: map[ExtensionKey]ExtensionHandler{{Namespace: "org.example/content", Version: "1"}: contentExtension()}}
	for _, name := range refs {
		t.Run(name, func(t *testing.T) {
			definitions, repo, provider := definitionHarness(t, policy)
			provider.documents["catalog:definition"] = authored(t, d)
			provider.badURI = "catalog:" + name
			if _, err := definitions.Index(t.Context(), "catalog:definition"); !errors.Is(err, ErrPinMismatch) {
				t.Fatal("bad dependency accepted", name, err)
			}
			if _, err := repo.Definition(t.Context(), d.DefinitionID, d.Revision); !errors.Is(err, fabricstore.ErrNotFound) {
				t.Fatal("partially verified definition indexed", err)
			}
		})
	}
	definitions, _, provider := definitionHarness(t, policy)
	provider.documents["catalog:definition"] = authored(t, d)
	indexed, err := definitions.Index(t.Context(), "catalog:definition")
	if err != nil {
		t.Fatal(err)
	}
	provider.badURI = "catalog:resource"
	if _, err := definitions.Load(t.Context(), indexed.Pin); !errors.Is(err, ErrPinMismatch) {
		t.Fatal("load used cached resource authority", err)
	}
	provider.badURI = ""
	provider.fileSkill = true
	if _, err := definitions.Load(t.Context(), indexed.Pin); !errors.Is(err, ErrPinMismatch) {
		t.Fatal("single-file skill pin accepted", err)
	}
	provider.fileSkill = false
	d.Extensions["org.example/content"].Data["unexpected"] = true
	provider.documents["catalog:definition"] = authored(t, d)
	if _, err := definitions.Index(t.Context(), "catalog:definition"); err == nil {
		t.Fatal("malformed negotiated extension accepted")
	}
}
func TestMissingExtensionHandlerRefusesConfiguration(t *testing.T) {
	_, repo, provider := definitionHarness(t, Policy{})
	_, err := NewDefinitionStore(repo, provider, Policy{KnownCapability: func(string) bool { return true }, Extensions: map[ExtensionKey]ExtensionHandler{{Namespace: "org.example/content", Version: "1"}: {Validate: func(agentdef.Extension) error { return nil }}}})
	if !errors.Is(err, ErrConfiguration) {
		t.Fatal(fmt.Sprint(err))
	}
}
