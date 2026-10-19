package teamruntime

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/definitionresolve"
	"github.com/hollis-labs/tether/internal/teamactivation"
	"github.com/hollis-labs/tether/internal/teamspolicy"
)

type ActivationInspector struct {
	Policy      *teamspolicy.Policy
	Definitions Definitions
	Service     *app.Service
	Enroller    *LegacyEnroller
}

func (i *ActivationInspector) baselineKey(l teamspolicy.Launch) string {
	raw, _ := json.Marshal(struct {
		Binding                    teamspolicy.Launch
		Launches                   []teamspolicy.Launch
		SessionFormation, Adoption bool
	}{l, i.Policy.Launches(), i.Policy.SessionFormation(), i.Policy.Adoption()})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// CaptureLoaded records the first resolution for this approved policy binding.
// INSERT OR IGNORE preserves that baseline across a daemon restart. Failure to
// observe is report-only: inspection must return unknown for the missing fact.
func (i *ActivationInspector) CaptureLoaded(ctx context.Context) {
	for _, l := range i.Policy.Launches() {
		if e, err := i.Service.InspectTeamLaunch(ctx, l.Launch); err == nil {
			e.PolicyDigest = "sha256:" + i.baselineKey(l)
			_ = i.Service.Store.RecordTeamEvidence(ctx, "loaded", i.baselineKey(l), e)
		}
	}
}

func (i *ActivationInspector) Inspect(ctx context.Context, q teamactivation.Query) (teamactivation.Evidence, error) {
	out := teamactivation.Evidence{Version: 1, Mapping: "legacy_catalog", Paused: i.Policy.Paused(), BaselineState: "unknown", Divergence: []string{}, Limitations: []string{"definition_behavior_and_harness_not_consumed", "argument_and_instruction_values_not_attested"}}
	if q.SessionID != "" {
		var raw []byte
		err := i.Enroller.db.QueryRowContext(ctx, `SELECT e.payload FROM team_port_intents s JOIN team_port_intents e ON e.intent_key=s.intent_key AND e.port_kind='enrollment' WHERE s.port_kind='session' AND s.state='done' AND CAST(s.payload AS TEXT)=json_quote(?) AND e.state='done'`, q.SessionID).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return out, errors.New("session has no retained team enrollment")
		}
		if err != nil {
			return out, err
		}
		var receipt enrollmentReceipt
		if err = json.Unmarshal(raw, &receipt); err != nil {
			return out, err
		}
		if q.LaunchID != "" && receipt.LaunchID != q.LaunchID {
			return out, errors.New("session launch does not match query")
		}
		q.LaunchID = receipt.LaunchID
		row, err := i.Service.Store.GetSession(q.SessionID)
		if err != nil {
			return out, err
		}
		plan, err := i.Service.Store.GetLaunchPlan(q.SessionID)
		if err != nil {
			return out, err
		}
		if plan == nil || plan.LaunchID != receipt.LaunchID || !plan.TeamMember {
			return out, errors.New("session launch plan mismatch")
		}
		out.Admitted = receipt.Evidence
		if out.Admitted != nil && strings.HasPrefix(out.Admitted.PolicyDigest, "sha256:") {
			out.AdmittedBaseline, _ = i.Service.Store.TeamEvidence(ctx, "loaded", strings.TrimPrefix(out.Admitted.PolicyDigest, "sha256:"))
		}
		out.Created = plan.ActivationEvidence
		out.Launched, _ = i.Service.Store.TeamEvidence(ctx, "launched", q.SessionID)
		out.Session = &teamactivation.Session{ID: row.ID, State: row.State, Pin: receipt.Pin, LaunchID: plan.LaunchID, Worker: row.WorkerID, Project: plan.ProjectID, RuntimeObserved: out.Launched != nil, DefinitionExecuted: false}
		if out.Created != nil {
			out.Session.PlanFingerprint = out.Created.Fingerprint
		}
	}

	var entry teamspolicy.Launch
	for _, l := range i.Policy.Launches() {
		if l.Launch == q.LaunchID {
			entry = l
			out.Loaded = true
			break
		}
	}
	if !out.Loaded {
		out.CurrentState = "current_unavailable"
		out.Divergence = append(out.Divergence, "current_unavailable")
		return finishActivation(out)
	}
	out.Pin = entry.Definition.Ref()
	out.PolicyDigest = "sha256:" + i.baselineKey(entry)
	verified, err := i.Definitions.Load(ctx, out.Pin)
	out.SourceVerified = err == nil && verified.Pin == out.Pin && verified.Definition != nil
	if out.SourceVerified {
		out.DefinitionPermissionProfile = verified.Definition.HarnessProfile.Permissions.Profile
	}
	if sources, ok := i.Definitions.(interface {
		InspectSource(context.Context, string) (definitionresolve.VerifiedDefinition, error)
	}); ok && entry.Source != "" {
		if installed, inspectErr := sources.InspectSource(ctx, entry.Source); inspectErr == nil {
			out.InstalledDefinitionPin = &installed.Pin
			if installed.Pin != out.Pin {
				out.Divergence = append(out.Divergence, "installed_differs_from_loaded")
			}
		}
	}
	if !out.SourceVerified {
		out.Divergence = append(out.Divergence, "definition_source_unverified")
	}
	disk, err := teamspolicy.Load(i.Service.CatalogRoot)
	if err == nil {
		for _, l := range disk.Launches() {
			if l.Launch == entry.Launch {
				out.InstalledPolicyMatches = l == entry
				break
			}
		}
	}
	if !out.InstalledPolicyMatches {
		out.Divergence = append(out.Divergence, "installed_differs_from_loaded")
	}
	if out.LoadedBaseline == nil {
		out.LoadedBaseline, _ = i.Service.Store.TeamEvidence(ctx, "loaded", i.baselineKey(entry))
	}
	out.Launch, err = i.Service.InspectTeamLaunch(ctx, q.LaunchID)
	if err != nil {
		out.CurrentState = "current_unavailable"
		out.Divergence = append(out.Divergence, "current_unavailable")
		return finishActivation(out)
	}
	out.CurrentState = "available"
	if !out.Launch.PlacementResolved {
		out.Divergence = append(out.Divergence, "placement_unresolved")
	}
	if out.Launch.InstalledSandboxPolicyDigest != "" && out.Launch.InstalledSandboxPolicyDigest != out.Launch.EffectiveSandboxPolicyDigest {
		out.Divergence = append(out.Divergence, "installed_differs_from_loaded")
	}
	if out.LoadedBaseline != nil {
		out.BaselineState = "recorded"
		if planFactsDiffer(&out.Launch, out.LoadedBaseline) {
			out.Divergence = append(out.Divergence, "launch_changed_since_load")
		}
	}
	if entry.Runtime != out.Launch.Brand {
		out.Divergence = append(out.Divergence, "policy_runtime_differs_from_launch")
	}
	if entry.Model != "" && entry.Model != out.Launch.CatalogModel {
		out.Divergence = append(out.Divergence, "policy_model_differs_from_catalog")
	}

	return finishActivation(out)
}

func finishActivation(out teamactivation.Evidence) (teamactivation.Evidence, error) {
	if planFactsDiffer(out.LoadedBaseline, out.Admitted) {
		out.Divergence = append(out.Divergence, "admitted_differs_from_loaded")
	}
	if planFactsDiffer(out.Admitted, out.Created) {
		out.Divergence = append(out.Divergence, "created_differs_from_admitted")
	}
	if planFactsDiffer(out.Admitted, out.Launched) {
		out.Divergence = append(out.Divergence, "executed_differs_from_admitted")
	}
	if privateSandboxDiffers(out.Created, out.Launched) {
		out.Divergence = append(out.Divergence, "sandbox_changed_between_create_and_launch")
	}
	if out.Session != nil && out.Loaded && out.Session.Pin != out.Pin {
		out.Divergence = append(out.Divergence, "session_admitted_under_different_pin")
	}
	if out.LoadedBaseline != nil {
		out.BaselineState = "recorded"
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return out, err
	}
	sum := sha256.Sum256(raw)
	out.EvidenceDigest = "sha256:" + hex.EncodeToString(sum[:])
	return out, nil
}

// InspectAdmission binds a current plan observation to the policy that admitted
// it, so history remains attributable even after that binding is removed.
func (i *ActivationInspector) InspectAdmission(ctx context.Context, id string) (teamactivation.Launch, error) {
	e, err := i.Service.InspectTeamLaunch(ctx, id)
	if err != nil {
		return e, err
	}
	for _, l := range i.Policy.Launches() {
		if l.Launch == id {
			e.PolicyDigest = "sha256:" + i.baselineKey(l)
			break
		}
	}
	return e, nil
}

// Recompute the comparable projection for retained observations, including ones
// written before metadata normalization. Missing historical facts stay unknown.
func planFactsDiffer(a, b *teamactivation.Launch) bool {
	if a == nil || b == nil || a.Version == "" || b.Version == "" || a.Fingerprint == "" || b.Fingerprint == "" {
		return false
	}
	return a.Digest() != b.Digest() || privateSandboxDiffers(a, b)
}
func privateSandboxDiffers(a, b *teamactivation.Launch) bool {
	return a != nil && b != nil && a.EffectiveSandboxPolicyDigest != "" && b.EffectiveSandboxPolicyDigest != "" && a.EffectiveSandboxPolicyDigest != b.EffectiveSandboxPolicyDigest
}
