package shimretire

import (
	"testing"
	"time"
)

func validProof() (Request, Snapshot, Observation, time.Time) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := Placement{"session", "instance", "launch", "revision", 1}
	u := Unit{"manager", "unit", "invocation", "attempt", "scope-revision"}
	j := JournalIdentity{"journal-root", "journal", p.Session, "journal:1", p.Generation}
	r := Request{Version: Version, OperationID: "retirement", ActorID: "operator", AuthorizationID: "authorization", AuthorizationRevision: "grant-revision", Reason: "operator_request", Placement: p, PolicyID: "keep", PolicyRevision: "policy-revision"}
	s := Snapshot{Placement: p, Backend: "systemd-user", Unit: u, Journal: j, InventoryRevision: "inventory", Descriptor: Artifact{RootID: "private-root", RelativePath: "launch.json", OwnerID: "owner", CustodyRevision: "custody", InventoryRevision: "inventory", Identity: FileIdentity{1, 2, "regular"}, Category: Descriptor}}
	o := Observation{Placement: p, Unit: u, Journal: j, Issuer: "host", HostBoot: "boot", Revision: "probe", Fence: "fence", Scope: "scope", ObservedAt: now.Add(-time.Second), ValidUntil: now.Add(time.Second), Submission: DrainedFenced, Containment: OwnedAllDescendants, Host: Absent, Descendants: Absent, Controller: Absent, JournalWriter: Absent}
	return r, s, o, now
}

func TestPositiveOwnedProof(t *testing.T) {
	r, s, o, now := validProof()
	if got := Verify(r, s, o, now); got.Outcome != Eligible {
		t.Fatalf("valid proof: %+v", got)
	}
}

func TestMissingEvidenceNeverProvesAbsence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Snapshot, *Observation)
	}{
		{"pending submit", func(s *Snapshot, o *Observation) { o.Submission = SubmissionPending }},
		{"unknown descendants", func(s *Snapshot, o *Observation) { o.Descendants = ExecutionUnknown }},
		{"live descendant", func(s *Snapshot, o *Observation) { o.Descendants = Present }},
		{"missing invocation", func(s *Snapshot, o *Observation) { s.Unit.Invocation = ""; o.Unit.Invocation = "" }},
		{"detached", func(s *Snapshot, o *Observation) { s.Backend = "detached" }},
		{"no containment", func(s *Snapshot, o *Observation) { o.Containment = ContainmentIncomplete }},
		{"expired proof", func(s *Snapshot, o *Observation) { o.ValidUntil = o.ObservedAt }},
		{"changed lineage", func(s *Snapshot, o *Observation) { o.Unit.Attempt = "other" }},
		{"busy writer", func(s *Snapshot, o *Observation) { o.JournalWriter = Present }},
		{"contradiction", func(s *Snapshot, o *Observation) { o.Contradictions = []string{"pending_job"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, s, o, now := validProof()
			tc.change(&s, &o)
			got := Verify(r, s, o, now)
			if got.Outcome == Eligible || len(got.Obligations) == 0 {
				t.Fatalf("unsafe proof: %+v", got)
			}
		})
	}
}
