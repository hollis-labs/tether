package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/catalogapply"
)

func TestCatalogApprovalGolden(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	makePlan := func(id, name, digest string) *catalogapply.Plan {
		return &catalogapply.Plan{ID: id, Name: name, Digest: digest, State: catalogapply.StatePlanned, ValidationOK: true, ExpiresAt: now.Add(time.Hour), TTLSeconds: 3600, SurvivesRestart: true, Files: []catalogapply.PlannedFile{{Path: "agents/" + name + ".yaml", Action: "create"}}, Diff: "+ reviewed file\n"}
	}
	for _, name := range []string{"single", "batch", "expired", "invalid", "rollback-expired", "privileged", "running"} {
		t.Run(name, func(t *testing.T) {
			p := makePlan("cp_one", "one", strings.Repeat("a", 64))
			plans := []*catalogapply.Plan{p}
			switch name {
			case "batch":
				plans = append(plans, makePlan("cp_two", "two", strings.Repeat("b", 64)))
			case "expired":
				p.ExpiresAt = now.Add(-time.Minute)
			case "invalid":
				p.State = catalogapply.StateInvalid
				p.ValidationOK = false
				p.ValidationError = "invalid YAML"
			case "rollback-expired":
				p.ExpiresAt = now.Add(-time.Minute)
				p.RollbackOf = "catalog-pre-one-20261120T120000Z"
			case "privileged":
				p.Privileged = []string{"agents/one.yaml: changes permissions"}
				p.Files[0].Privileged = true
			case "running":
				p.RunningSessions = []string{"session-one"}
			}
			var out bytes.Buffer
			err := printApprovalReview(&out, plans, now, false, false)
			passwordCalls := 0
			if err == nil {
				token := approvalFingerprint(plans)
				pass, readErr := readApproval(&out, strings.NewReader(token+"\n"), token, func() ([]byte, error) { passwordCalls++; return []byte("synthetic test input"), nil })
				clear(pass)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if passwordCalls != 1 {
					t.Fatalf("passphrase prompts: %d", passwordCalls)
				}
			} else {
				out.WriteString("ERROR: " + err.Error() + "\n")
			}
			golden, err := os.ReadFile(filepath.Join("testdata", "catalog-approval-"+name+".golden"))
			if err != nil {
				t.Fatal(err)
			}
			// Golden files spell otherwise invisible trailing prompt spaces as \x20.
			want := strings.ReplaceAll(string(golden), `\x20`, " ")
			if out.String() != want {
				t.Fatalf("golden differs (-want +got):\nwant:\n%s\ngot:\n%s", golden, out.String())
			}
		})
	}
}

func TestApprovalFingerprintSortedAndBoundToFullDigests(t *testing.T) {
	a := &catalogapply.Plan{Digest: strings.Repeat("a", 64)}
	b := &catalogapply.Plan{Digest: strings.Repeat("b", 64)}
	if approvalFingerprint([]*catalogapply.Plan{a, b}) != approvalFingerprint([]*catalogapply.Plan{b, a}) {
		t.Fatal("order changes fingerprint")
	}
	if got := approvalFingerprint([]*catalogapply.Plan{a, b}); got != "5e9ae866add9" {
		t.Fatalf("canonical fingerprint: %s", got)
	}
	before := approvalFingerprint([]*catalogapply.Plan{a, b})
	b.Digest = strings.Repeat("b", 63) + "c"
	if before == approvalFingerprint([]*catalogapply.Plan{a, b}) {
		t.Fatal("fingerprint ignores digest suffix")
	}
	if approvalFingerprint([]*catalogapply.Plan{a}) != strings.Repeat("a", 12) {
		t.Fatal("single token changed")
	}
}

func TestApprovalWrongConfirmationNeverPromptsPassphrase(t *testing.T) {
	var out bytes.Buffer
	calls := 0
	if _, err := readApproval(&out, strings.NewReader("wrong\n"), "aaaaaaaaaaaa", func() ([]byte, error) { calls++; return nil, nil }); err == nil {
		t.Fatal("wrong token accepted")
	}
	if calls != 0 || strings.Contains(out.String(), "Approval passphrase") {
		t.Fatal("passphrase requested before confirmation")
	}
}
