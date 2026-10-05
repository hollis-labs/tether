package shimretire

import (
	"path"
	"strings"
	"time"
	"unicode/utf8"
)

const Version = "shim.retirement.v1"

type Placement struct {
	Session, Instance, OperationKey, Revision string
	Generation                                uint64
}

type Request struct {
	Version, OperationID, ActorID, AuthorizationID, AuthorizationRevision, Reason string
	Placement                                                                     Placement
	PolicyID, PolicyRevision                                                      string
}

type FileIdentity struct {
	Device, Inode uint64
	Kind          string
}

type Artifact struct {
	RootID, RelativePath, OwnerID, CustodyRevision, InventoryRevision string
	Identity                                                          FileIdentity
	Category                                                          Category
	Size                                                              uint64
}

type Category string

const (
	Descriptor Category = "launch_descriptor"
	Staging    Category = "commit_staging"
	Journal    Category = "journal"
	Bridge     Category = "bridge_checkpoint"
	HostLog    Category = "host_log"
	Sandbox    Category = "sandbox_temp"
)

type Unit struct {
	Manager, Name, Invocation, Attempt, ScopeRevision string
}
type JournalIdentity struct {
	RootID, ID, Session, HighWater string
	Generation                     uint64
}
type Snapshot struct {
	Placement         Placement
	Backend           string
	Unit              Unit
	Journal           JournalIdentity
	Descriptor        Artifact
	InventoryRevision string
	Retired           bool
}

type SubmissionState string

const (
	DrainedFenced     SubmissionState = "drained_fenced"
	SubmissionPending SubmissionState = "pending"
	SubmissionUnknown SubmissionState = "unknown"
)

type Containment string

const (
	OwnedAllDescendants    Containment = "owned_all_descendants"
	ContainmentIncomplete  Containment = "incomplete"
	ContainmentUnsupported Containment = "unsupported"
)

type ExecutionState string

const (
	Absent           ExecutionState = "absent"
	Present          ExecutionState = "present"
	ExecutionUnknown ExecutionState = "unknown"
)

// Observation is supplied by the trusted host while its submission fence and
// complete lifecycle/custody lease remain held. It is never a user assertion.
type Observation struct {
	Placement                                    Placement
	Unit                                         Unit
	Journal                                      JournalIdentity
	Issuer, HostBoot, Revision, Fence, Scope     string
	ObservedAt, ValidUntil                       time.Time
	Submission                                   SubmissionState
	Containment                                  Containment
	Host, Descendants, Controller, JournalWriter ExecutionState
	Contradictions                               []string
}

type Outcome string

const (
	Eligible              Outcome = "eligible"
	RetainedUnknown       Outcome = "retained_unknown"
	UnsupportedProof      Outcome = "unsupported_proof"
	RefusedPossibleChild  Outcome = "refused_possible_child"
	RefusedIdentity       Outcome = "refused_identity"
	RetiredCleanupPending Outcome = "retired_cleanup_pending"
	RetiredStatePending   Outcome = "retired_state_pending"
	Retired               Outcome = "retired"
	AlreadyRetired        Outcome = "already_retired"
)

type Result struct {
	Outcome     Outcome
	Code        string
	Obligations []string
}

func Verify(r Request, s Snapshot, o Observation, now time.Time) Result {
	refuse := func(out Outcome, code string) Result {
		return Result{Outcome: out, Code: code, Obligations: []string{"placement_unknown"}}
	}
	if !validRequest(r) || s.Placement != r.Placement || o.Placement != r.Placement || s.Unit != o.Unit || s.Journal != o.Journal {
		return refuse(RefusedIdentity, "identity_changed")
	}
	if s.Backend != "systemd-user" || !validUnit(s.Unit) || o.Containment == ContainmentUnsupported {
		return refuse(UnsupportedProof, "containment_unproven")
	}
	if !text(s.InventoryRevision, s.Descriptor.RootID, s.Descriptor.OwnerID, s.Descriptor.CustodyRevision, s.Descriptor.InventoryRevision) || s.Descriptor.InventoryRevision != s.InventoryRevision || s.Descriptor.Category != Descriptor || s.Descriptor.Identity.Kind != "regular" || s.Descriptor.Identity.Inode == 0 || !relative(s.Descriptor.RelativePath) || !text(s.Journal.RootID, s.Journal.ID, s.Journal.HighWater) || s.Journal.Session != r.Placement.Session || s.Journal.Generation != r.Placement.Generation {
		return refuse(RefusedIdentity, "custody_unproven")
	}
	if o.Host == Present || o.Descendants == Present || o.Controller == Present || o.JournalWriter == Present || o.Submission == SubmissionPending {
		return refuse(RefusedPossibleChild, "possible_child")
	}
	if !text(o.Issuer, o.HostBoot, o.Revision, o.Fence, o.Scope) || now.IsZero() || o.ObservedAt.IsZero() || o.ObservedAt.After(now) || !o.ValidUntil.After(now) || o.Submission != DrainedFenced || o.Containment != OwnedAllDescendants || o.Host != Absent || o.Descendants != Absent || o.Controller != Absent || o.JournalWriter != Absent || len(o.Contradictions) != 0 {
		return refuse(RetainedUnknown, "absence_unproved")
	}
	return Result{Outcome: Eligible, Code: "owned_absence_verified"}
}

func text(values ...string) bool {
	for _, s := range values {
		if s == "" || len(s) > 4096 || !utf8.ValidString(s) || strings.ContainsAny(s, "\x00\r\n") {
			return false
		}
	}
	return true
}
func relative(s string) bool {
	return text(s) && s != "." && path.Clean(s) == s && !strings.HasPrefix(s, "/") && s != ".." && !strings.HasPrefix(s, "../") && !strings.Contains(s, "\\")
}
func validUnit(u Unit) bool { return text(u.Manager, u.Name, u.Invocation, u.Attempt, u.ScopeRevision) }
func validRequest(r Request) bool {
	return r.Version == Version && r.Reason == "operator_request" && r.Placement.Generation > 0 && text(r.OperationID, r.ActorID, r.AuthorizationID, r.AuthorizationRevision, r.Placement.Session, r.Placement.Instance, r.Placement.OperationKey, r.Placement.Revision, r.PolicyID, r.PolicyRevision)
}
