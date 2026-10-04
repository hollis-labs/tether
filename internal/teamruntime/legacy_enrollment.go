package teamruntime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/tether/internal/definitionresolve"
	"github.com/hollis-labs/tether/internal/fabricstore"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/teamhost"
	"github.com/hollis-labs/tether/internal/teamstore"
)

const enrollmentSubstrate = "tether-team"

// Definitions verifies content and every reference against the exact pin.
// definitionresolve.DefinitionStore is the production implementation.
type Definitions interface {
	Load(context.Context, mesh.DefinitionRef) (definitionresolve.VerifiedDefinition, error)
}

// LaunchTarget is an operator-selected legacy catalog launch, bounded by a
// verified definition pin. It is execution configuration, never registry content.
type LaunchTarget struct{ LaunchID string }
type enrollmentReceipt struct {
	Enrollment teamhost.Enrollment
	LaunchID   string
	Pin        mesh.DefinitionRef
}
type LegacyEnroller struct {
	receipts
	registry    *registry.Service
	definitions Definitions
	targets     map[mesh.DefinitionRef]LaunchTarget
}

func NewLegacyEnroller(db *sql.DB, s *teamstore.Store, directory *registry.Service, definitions Definitions, targets map[mesh.DefinitionRef]LaunchTarget) (*LegacyEnroller, error) {
	if db == nil || s == nil || directory == nil || definitions == nil {
		return nil, errors.New("team enroller: database, registry and definition verifier required")
	}
	copyTargets := map[mesh.DefinitionRef]LaunchTarget{}
	for pin, target := range targets {
		if pin.ID == "" || pin.Revision == "" || pin.Digest == "" || target.LaunchID == "" {
			return nil, errors.New("team enroller: exact pin and launch required")
		}
		copyTargets[pin] = target
	}
	return &LegacyEnroller{receipts: receipts{db, s}, registry: directory, definitions: definitions, targets: copyTargets}, nil
}
func (e *LegacyEnroller) Ensure(ctx context.Context, in teamhost.EnrollmentRequest) (teamhost.Enrollment, error) {
	unlock := e.lock("enrollment", in.IntentKey)
	defer unlock()
	r, err := e.reserve(ctx, "enrollment", in.IntentKey, in)
	if err != nil {
		return teamhost.Enrollment{}, err
	}
	if len(r.payload) > 0 {
		var saved enrollmentReceipt
		err = json.Unmarshal(r.payload, &saved)
		return saved.Enrollment, err
	}
	fresh := in.Provision.Slot.Resolution == teams.Fresh
	if fresh && in.Actor != "" {
		return teamhost.Enrollment{}, teams.ErrProvisionFailed
	}
	pin := in.Provision.Slot.Definition
	var profile registry.Profile
	if !fresh {
		if in.Actor == "" || in.Actor != in.Provision.Identity {
			return teamhost.Enrollment{}, teams.ErrProvisionFailed
		}
		profile, err = e.registry.Lookup(ctx, string(in.Actor))
		if err != nil {
			return teamhost.Enrollment{}, classifyEnrollment(err)
		}
		var enrolledPin mesh.DefinitionRef
		if err = json.Unmarshal([]byte(profile.Props["team_definition"]), &enrolledPin); err != nil {
			return teamhost.Enrollment{}, teams.ErrProvisionFailed
		}
		if pin.ID == "" {
			pin = enrolledPin
		} else if pin != enrolledPin {
			return teamhost.Enrollment{}, teams.ErrProvisionFailed
		}
	}
	verified, err := e.definitions.Load(ctx, pin)
	if err != nil {
		return teamhost.Enrollment{}, classifyEnrollment(err)
	}
	if verified.Pin != pin || verified.Definition == nil {
		return teamhost.Enrollment{}, teams.ErrProvisionFailed
	}
	target, ok := e.targets[pin]
	if !ok {
		return teamhost.Enrollment{}, fmt.Errorf("%w: no legacy execution target for pin", teams.ErrProvisionFailed)
	}
	if fresh {
		pinJSON, err := json.Marshal(pin)
		if err != nil {
			return teamhost.Enrollment{}, err
		}
		profile, _, err = e.registry.RegisterIdempotent(ctx, registry.KindAgent, registry.Profile{DisplayName: verified.Definition.Name, Props: map[string]string{"team_definition": string(pinJSON), "team_provenance": r.nonce}}, enrollmentSubstrate, r.nonce)
		if err != nil {
			return teamhost.Enrollment{}, classifyEnrollment(err)
		}
		if !matchesProvenance(profile, r.nonce) {
			return teamhost.Enrollment{}, teams.ErrProvisionFailed
		}

	}
	if profile.Kind != registry.KindAgent || profile.Status != registry.StatusActive {
		return teamhost.Enrollment{}, teams.ErrProvisionFailed
	}
	var retainedPin mesh.DefinitionRef
	if err = json.Unmarshal([]byte(profile.Props["team_definition"]), &retainedPin); err != nil || retainedPin != pin {
		return teamhost.Enrollment{}, teams.ErrProvisionFailed
	}
	if err = e.recordAcquisition(context.WithoutCancel(ctx), in.IntentKey, r.nonce, profile.URN); err != nil {
		return teamhost.Enrollment{}, err
	}
	enrollment := teamhost.Enrollment{Actor: mesh.URN(profile.URN), AgentID: profile.URN, Kind: mesh.ActorAgent, Ephemeral: fresh}
	for _, capability := range verified.Definition.Capabilities {
		if capability.ID == mesh.SpawnCapabilityURI {
			enrollment.SpawnCapable = true
		}
	}
	saved := enrollmentReceipt{enrollment, target.LaunchID, pin}
	if err = e.complete(ctx, "enrollment", in.IntentKey, saved); err != nil {
		return teamhost.Enrollment{}, errors.Join(err, e.cleanup(context.WithoutCancel(ctx), in.IntentKey))
	}
	return enrollment, nil
}
func classifyEnrollment(err error) error {
	if errors.Is(err, registry.ErrNotFound) || errors.Is(err, registry.ErrInvalidRequest) || errors.Is(err, definitionresolve.ErrPinMismatch) || errors.Is(err, definitionresolve.ErrContent) || errors.Is(err, fabricstore.ErrNotFound) {
		return fmt.Errorf("%w: %w", teams.ErrProvisionFailed, err)
	}
	return err
}
func (e *LegacyEnroller) AcquireBinding(ctx context.Context, key string, actor mesh.URN) error {
	unlock := e.lock("enrollment", key)
	defer unlock()
	r, err := e.read(ctx, "enrollment", key)
	if err != nil {
		return err
	}
	if r.ended != "" || r.bindingEnded {
		return teams.ErrDenied
	}
	var saved enrollmentReceipt
	if err = json.Unmarshal(r.payload, &saved); err != nil {
		return err
	}
	if saved.Enrollment.Actor != actor {
		return teams.ErrConflict
	}
	current, err := e.registry.CurrentBinding(ctx, string(actor))
	if err == nil && current.HostID == "team" && current.AttemptID == r.bindingSecret && current.Visibility == registry.VisibilityTetherHosted {
		return nil
	}
	if err == nil {
		return teams.ErrUnavailable
	}
	if !errors.Is(err, registry.ErrBindingNotFound) {
		return err
	}
	_, err = e.registry.LeaseBindingUnlessVisibility(ctx, string(actor), "team-intent:"+r.bindingSecret, "team", r.bindingSecret, nil, registry.VisibilityTetherHosted, 0, registry.VisibilityPrivateLocal, registry.VisibilityPublishedLocal, registry.VisibilityTetherHosted)
	if err != nil {
		return err
	}
	retained, err := e.read(ctx, "enrollment", key)
	if err != nil {
		return err
	}
	if retained.ended != "" || retained.bindingEnded {
		return errors.Join(teams.ErrDenied, e.releaseBinding(context.WithoutCancel(ctx), key))
	}
	return nil
}
func (e *LegacyEnroller) ReleaseBinding(ctx context.Context, key string) error {
	if err := e.end(ctx, "enrollment", key, "", true); err != nil {
		return err
	}
	unlock := e.lock("enrollment", key)
	defer unlock()
	return e.releaseBinding(ctx, key)
}
func (e *LegacyEnroller) releaseBinding(ctx context.Context, key string) error {
	r, err := e.read(ctx, "enrollment", key)
	if err != nil {
		return err
	}
	if r.bindingSecret == "" {
		return nil
	}
	// The receipt-private binding secret and recorded actor fence cleanup;
	// the registry-visible enrollment nonce grants no binding ownership.
	_, err = e.db.ExecContext(ctx, `UPDATE runtime_bindings SET revoked_at=COALESCE(revoked_at,?),updated_at=? WHERE host_id='team' AND visibility='tether-hosted' AND attempt_id=? AND target_urn=?`, time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano), r.bindingSecret, r.acquired)
	return err
}
func (e *LegacyEnroller) Release(ctx context.Context, key string) error {
	return e.finish(ctx, key, "release")
}
func (e *LegacyEnroller) Retire(ctx context.Context, key string) error {
	return e.finish(ctx, key, "retire")
}
func (e *LegacyEnroller) finish(ctx context.Context, key, mode string) error {
	if err := e.end(ctx, "enrollment", key, mode, false); err != nil {
		return err
	}
	unlock := e.lock("enrollment", key)
	defer unlock()
	return e.cleanup(ctx, key)
}
func (e *LegacyEnroller) cleanup(ctx context.Context, key string) error {
	r, err := e.read(ctx, "enrollment", key)
	if err != nil {
		return err
	}
	if r.ended == "" {
		return nil
	}
	if err = e.releaseBinding(ctx, key); err != nil {
		return err
	}
	if r.ended == "retire" && len(r.request) > 0 && r.nonce != "" {
		var request teamhost.EnrollmentRequest
		if err = json.Unmarshal(r.request, &request); err != nil {
			return err
		}
		if request.Provision.Slot.Resolution == teams.Fresh {
			// Recover a lost acknowledgement only through the already committed nonce.
			if r.acquired == "" {
				profile, lookupErr := e.registry.LookupBy(ctx, registry.KindAgent, r.nonce, enrollmentSubstrate)
				if lookupErr != nil && !errors.Is(lookupErr, registry.ErrNotFound) {
					return lookupErr
				}
				if lookupErr == nil && matchesProvenance(profile, r.nonce) {
					if err = e.recordAcquisition(ctx, key, r.nonce, profile.URN); err != nil {
						return err
					}
					r.acquired = profile.URN
				}
			}
			if r.acquired != "" {
				profile, lookupErr := e.registry.Lookup(ctx, r.acquired)
				if lookupErr != nil && !errors.Is(lookupErr, registry.ErrNotFound) {
					return lookupErr
				}
				if lookupErr == nil && matchesProvenance(profile, r.nonce) {
					if _, err = e.registry.Deregister(ctx, r.acquired); err != nil && !errors.Is(err, registry.ErrNotFound) {
						return err
					}
				}
			}
		}
	}
	return e.cleaned(ctx, "enrollment", key)
}

func matchesProvenance(profile registry.Profile, nonce string) bool {
	if nonce == "" || profile.Props["team_provenance"] != nonce {
		return false
	}
	for _, id := range profile.ExternalIDs {
		if id.Substrate == enrollmentSubstrate && id.ExternalID == nonce {
			return true
		}
	}
	return false
}
