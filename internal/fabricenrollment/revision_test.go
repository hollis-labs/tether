package fabricenrollment

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/tether/internal/definitionresolve"
	"github.com/hollis-labs/tether/internal/fabricstore"
)

func TestDirectoryRealStoreStaleSourceAndDependencyIsolation(t *testing.T) {
	for _, mutation := range []string{"presentation", "source removed", "dependency removed", "root removed", "root replaced", "source permission"} {
		t.Run(mutation, func(t *testing.T) {
			if mutation == "source permission" && os.Geteuid() == 0 {
				t.Skip("root bypasses file permission refusal")
			}
			s, repo, _ := harness(t)
			root := t.TempDir()
			write := func(name, body string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			write("dependency.txt", "fixture dependency")
			local, err := definitionresolve.NewLocalContent(root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = local.Close() })
			dep, err := local.Pin(t.Context(), "catalog:dependency.txt")
			if err != nil {
				t.Fatal(err)
			}
			body := func(id string) string {
				return fmt.Sprintf(`---
schema_version: "2"
definition_id: %s
revision: r1
name: %s
description: Integration fixture
behavior:
  purpose: Review a synthetic fixture
capabilities:
  - id: review
    description: Review source
requirements: {}
harness_profile:
  context: {}
  permissions:
    profile: review
continuity:
  mode: durable
---
Authored instructions.
`, id, id)
			}
			stale := body("stale")
			if mutation == "dependency removed" {
				stale = strings.Replace(stale, "requirements: {}", "requirements:\n  resources:\n    - uri: catalog:dependency.txt\n      digest: "+dep.Digest, 1)
			}
			write("stale.md", stale)
			write("healthy.md", body("healthy"))
			defs, err := definitionresolve.NewDefinitionStore(repo, local, definitionresolve.Policy{KnownCapability: func(string) bool { return true }})
			if err != nil {
				t.Fatal(err)
			}
			s.definitions = defs
			goodURN := mesh.URN("msg://agent/example/healthy")
			for _, item := range []struct {
				path string
				urn  mesh.URN
			}{{"stale.md", actorURN}, {"healthy.md", goodURN}} {
				verified, err := defs.Index(t.Context(), "catalog:"+item.path)
				if err != nil {
					t.Fatal(err)
				}
				req := request(item.urn)
				req.Definition = &verified.Pin
				if err := s.EnrollActor(t.Context(), owner, req); err != nil {
					t.Fatal(err)
				}
			}
			switch mutation {
			case "root removed", "root replaced":
				if err := os.RemoveAll(root); err != nil {
					t.Fatal(err)
				}
				if mutation == "root replaced" {
					if err := os.Mkdir(root, 0700); err != nil {
						t.Fatal(err)
					}
				}
			case "source permission":
				path := filepath.Join(root, "stale.md")
				if err := os.Chmod(path, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(path, 0600) })
			case "presentation":
				write("stale.md", strings.Replace(stale, "description: Integration fixture", "description: Changed presentation", 1))
			case "source removed":
				if err := os.Remove(filepath.Join(root, "stale.md")); err != nil {
					t.Fatal(err)
				}
			case "dependency removed":
				if err := os.Remove(filepath.Join(root, "dependency.txt")); err != nil {
					t.Fatal(err)
				}
			}
			page, err := s.Directory(t.Context(), owner, owner, "", 100)
			if mutation == "root removed" || mutation == "root replaced" || mutation == "source permission" {
				want := fs.ErrNotExist
				if mutation == "source permission" {
					want = fs.ErrPermission
				}
				if !errors.Is(err, want) || errors.Is(err, definitionresolve.ErrContent) {
					t.Fatal("operational root or permission fault omitted", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, row := range page.Records {
				if row.URN == actorURN {
					t.Fatal("stale row published")
				}
				if row.URN == goodURN {
					found = true
				}
			}
			if !found {
				t.Fatal("stale content blocked healthy agent")
			}
		})
	}
}

func TestAuthorizerFieldsAndOpaqueRefusals(t *testing.T) {
	s, _, _ := harness(t)
	var calls []Authorization
	s.authorize = func(_ context.Context, a Authorization) error {
		if a.Definition != nil {
			p := *a.Definition
			a.Definition = &p
		}
		calls = append(calls, a)
		return nil
	}
	enroll(t, s, actorURN)
	if err := s.RebindAgent(t.Context(), owner, actorURN, nextPin, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.RetireActor(t.Context(), owner, actorURN, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Directory(t.Context(), owner, owner, "", 100); err != nil {
		t.Fatal(err)
	}
	src, maps := manifest()
	for i := range src.Identities {
		src.Identities[i].Actor.URN += "-import"
		maps[i].ActorURN = src.Identities[i].Actor.URN
	}
	if _, err := s.PreviewImport(t.Context(), owner, src, maps); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyImport(t.Context(), owner, src, maps, "urn:approval:fixture"); err != nil {
		t.Fatal(err)
	}
	expected := []Authorization{
		{Caller: owner, Owner: owner, Target: actorURN, Action: Enroll, Definition: &pin},
		{Caller: owner, Owner: owner, Target: actorURN, Action: Rebind, Definition: &nextPin},
		{Caller: owner, Owner: owner, Target: actorURN, Action: Retire},
		{Caller: owner, Owner: owner, Target: owner, Action: ReadDirectory},
	}
	for n := 0; n < 2; n++ {
		for _, i := range src.Identities {
			var p *mesh.DefinitionRef
			for _, m := range maps {
				if m.Key == i.Key {
					p = m.Definition
				}
			}
			expected = append(expected, Authorization{Caller: owner, Owner: i.Owner, Target: i.Actor.URN, Action: Import, Definition: p, Source: src.Source})
		}
	}
	if !reflect.DeepEqual(calls, expected) {
		t.Fatalf("authorization fields: got %#v want %#v", calls, expected)
	}
	s.authorize = func(context.Context, Authorization) error { return errors.New("private policy cause") }
	for _, urn := range []mesh.URN{actorURN, "msg://agent/example/absent"} {
		for _, err := range []error{s.RebindAgent(t.Context(), owner, urn, pin, 1), s.RetireActor(t.Context(), owner, urn, 1)} {
			if !errors.Is(err, ErrDenied) || err.Error() != ErrDenied.Error() || strings.Contains(err.Error(), "private policy") {
				t.Fatal("authorization oracle", err)
			}
		}
	}
}

func TestDirectoryPolicyPaginationFreshnessAndOmission(t *testing.T) {
	s, _, defs := harness(t)
	enroll(t, s, actorURN)
	second := mesh.URN("msg://agent/example/second")
	enroll(t, s, second)
	for _, limit := range []int{0, 101} {
		if _, err := s.Directory(t.Context(), owner, owner, "", limit); !errors.Is(err, fabricstore.ErrInvalid) {
			t.Fatal(err)
		}
	}
	first, err := s.Directory(t.Context(), owner, owner, "", 1)
	if err != nil || first.Cursor != actorURN {
		t.Fatal(first, err)
	}
	rest, err := s.Directory(t.Context(), owner, owner, first.Cursor, 1)
	if err != nil || rest.Cursor != second {
		t.Fatal(rest, err)
	}
	for _, row := range rest.Records {
		if row.URN != second {
			t.Fatal("after cursor repeated row")
		}
	}
	s.authorize = func(_ context.Context, a Authorization) error {
		if a.Target == actorURN {
			return ErrDenied
		}
		return nil
	}
	page, err := s.Directory(t.Context(), owner, owner, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range page.Records {
		if r.URN == actorURN {
			t.Fatal("denied row published")
		}
	}
	if !directoryHas(page, second) {
		t.Fatal("allowed row omitted")
	}
	s.authorize = func(_ context.Context, a Authorization) error {
		if a.Target == actorURN {
			return context.DeadlineExceeded
		}
		return nil
	}
	if _, err := s.Directory(t.Context(), owner, owner, "", 100); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	s.authorize = func(context.Context, Authorization) error { return nil }
	start := now
	clock := start
	s.now = func() time.Time { return clock }
	defs.hook = func() { clock = start.Add(30 * time.Second) }
	page, err = s.Directory(t.Context(), owner, owner, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range page.Records {
		if !r.VerifiedAt.Equal(start) || !r.ValidUntil.Equal(start.Add(time.Minute)) {
			t.Fatal("freshness starts after content I/O", r)
		}
	}
	defs.hook = nil
	for _, value := range []definitionresolve.VerifiedDefinition{{Pin: nextPin, Definition: defs.values[pin].Definition}, {Pin: pin}} {
		s.definitions = definitionsFunc(func(context.Context, mesh.DefinitionRef) (definitionresolve.VerifiedDefinition, error) {
			return value, nil
		})
		page, err = s.Directory(t.Context(), owner, owner, "", 100)
		if err != nil || directoryHas(page, actorURN) || directoryHas(page, second) {
			t.Fatal(page, err)
		}
	}
	s.definitions = defs
	if err := s.RetireActor(t.Context(), owner, actorURN, 1); err != nil {
		t.Fatal(err)
	}
	page, err = s.Directory(t.Context(), owner, owner, "", 100)
	if err != nil || directoryHas(page, actorURN) || !directoryHas(page, second) {
		t.Fatal(page, err)
	}
	s.advertise = func(context.Context, Authorization, definitionresolve.VerifiedDefinition) (Publication, error) {
		return Publication{Publish: true, Capabilities: []string{"unknown", "review"}}, nil
	}
	page, err = s.Directory(t.Context(), owner, owner, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range page.Records {
		if !reflect.DeepEqual(r.Capabilities, []string{"review"}) {
			t.Fatal(r)
		}
	}
	s.advertise = func(context.Context, Authorization, definitionresolve.VerifiedDefinition) (Publication, error) {
		return Publication{Publish: true, Capabilities: []string{"unknown", "unknown"}}, nil
	}
	if _, err := s.Directory(t.Context(), owner, owner, "", 100); !errors.Is(err, fabricstore.ErrInvalid) {
		t.Fatal("duplicate IDs accepted", err)
	}
}
func directoryHas(page DirectoryPage, urn mesh.URN) bool {
	for _, r := range page.Records {
		if r.URN == urn {
			return true
		}
	}
	return false
}

func TestLifecycleOutboxRollbackAndLeaseBetweenReadAndWrite(t *testing.T) {
	for _, op := range []Action{Rebind, Retire} {
		t.Run(string(op), func(t *testing.T) {
			s, repo, _ := harness(t)
			enroll(t, s, actorURN)
			beforeActor, err := repo.Actor(t.Context(), actorURN)
			if err != nil {
				t.Fatal(err)
			}
			beforeAgent, err := repo.Agent(t.Context(), actorURN)
			if err != nil {
				t.Fatal(err)
			}
			mutate := func() error {
				if op == Rebind {
					return s.RebindAgent(t.Context(), owner, actorURN, nextPin, 1)
				}
				return s.RetireActor(t.Context(), owner, actorURN, 1)
			}
			s.now = func() time.Time { return time.Time{} }
			if err := mutate(); !errors.Is(err, fabricstore.ErrInvalid) {
				t.Fatal(err)
			}
			afterActor, err := repo.Actor(t.Context(), actorURN)
			if err != nil {
				t.Fatal(err)
			}
			afterAgent, err := repo.Agent(t.Context(), actorURN)
			if err != nil {
				t.Fatal(err)
			}
			if afterActor != beforeActor || afterAgent != beforeAgent {
				t.Fatal("outbox failure left lifecycle mutation")
			}
			s.now = func() time.Time { return now }
			s.authorize = func(context.Context, Authorization) error { bind(t, repo, now.Add(time.Minute)); return nil }
			if err := mutate(); !errors.Is(err, ErrBound) {
				t.Fatal("new lease missed inside writer", err)
			}
		})
	}
}
func TestImportTextBoundsAndApprovalUTF8(t *testing.T) {
	s, repo, _ := harness(t)
	src, maps := manifest()
	for _, approval := range []string{"invalid\xff", strings.Repeat("x", maxImportTextBytes+1)} {
		if _, err := s.ApplyImport(t.Context(), owner, src, maps, approval); !errors.Is(err, ErrManifest) {
			t.Fatal(err)
		}
	}
	requireNotEnrolled(t, repo, src.Identities[0].Actor.URN)
	for _, field := range []string{"source", "key"} {
		copySource, copyMappings := manifest()
		if field == "source" {
			copySource.Source = strings.Repeat("s", maxImportTextBytes+1)
		} else {
			copySource.Identities[0].Key = strings.Repeat("k", maxImportTextBytes+1)
			copyMappings[0].Key = copySource.Identities[0].Key
		}
		if _, err := s.PreviewImport(t.Context(), owner, copySource, copyMappings); !errors.Is(err, ErrManifest) {
			t.Fatal(field, err)
		}
	}
}
