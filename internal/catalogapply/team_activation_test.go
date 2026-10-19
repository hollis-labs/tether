package catalogapply

import (
	"strings"
	"testing"

	"github.com/hollis-labs/libs/mesh/core/agentdef"
	"github.com/hollis-labs/tether/internal/teamspolicy"
	"gopkg.in/yaml.v3"
)

func TestTeamActivationProposalPreservesPolicyAndRequiresExistingApproval(t *testing.T) {
	e := newEngine(t)
	d := agentdef.Definition{SchemaVersion: "2", DefinitionID: "example", Revision: "r2", Name: "example", Description: "Example definition", Behavior: agentdef.Behavior{Purpose: "Help with a bounded task"}, HarnessProfile: agentdef.HarnessProfile{Permissions: agentdef.PermissionProfile{Profile: "review"}}, Continuity: agentdef.Continuity{Mode: agentdef.Durable}, Body: "Follow the assignment."}
	front, err := yaml.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	source := "---\n" + string(front) + "---\n" + d.Body + "\n"
	write(t, e.Root, "team-definitions/example.md", source)
	id, rev, digest, err := teamspolicy.PinOfBytes([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	original := "version: 1\nsession_formation: true\nlaunches:\n- launch: general\n  runtime: cli\n  model: existing-model\n  definition: {id: example, revision: r1, digest: old}\n- launch: other\n  runtime: other\n  definition: {id: other, revision: r1, digest: other}\n"
	write(t, e.Root, "teams-policy.yaml", original)
	req := TeamActivationRequest{Name: "review-pin", LaunchID: "general", Expected: teamspolicy.Pin{ID: id, Revision: "r1", Digest: "old"}, Definition: teamspolicy.Pin{ID: id, Revision: rev, Digest: digest}, Source: "catalog:team-definitions/example.md"}
	p, err := e.PlanTeamActivation(t.Context(), req, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if read(t, e.Root, "teams-policy.yaml") != original {
		t.Fatal("planning installed live policy")
	}
	if len(p.Privileged) == 0 {
		t.Fatal("pin activation not privileged")
	}
	if _, err = e.Apply(t.Context(), p.ID); err == nil {
		t.Fatal("applied without approval")
	}
	var proposedRaw string
	for _, f := range p.Files {
		if f.Path == "teams-policy.yaml" {
			proposedRaw = f.content
		}
	}
	proposed, err := teamspolicy.Parse([]byte(proposedRaw))
	if err != nil {
		t.Fatal(err)
	}
	if !proposed.SessionFormation() || proposed.Launches()[1].Launch != "other" || proposed.Launches()[0].Model != "existing-model" {
		t.Fatal("targeted proposal rewrote unrelated policy")
	}
	approve(t, e, p)
	result, err := e.Apply(t.Context(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Activation, "restart") {
		t.Fatal("activation bypassed restart requirement", result.Activation)
	}
	if _, err = e.PlanTeamActivation(t.Context(), req, Options{}); err == nil {
		t.Fatal("stale expected pin accepted")
	}
}
