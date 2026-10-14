// Package teamspolicy is the operator-provisioned activation policy for team
// formation (Stage A). It is read once from the catalog at daemon start and is
// default-deny: without a valid file no team can be formed. Caller-authored
// definitions and catalog launch names grant nothing on their own; only exact
// definition pins listed in the file are trusted, each bound to one launch id.
package teamspolicy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sync/atomic"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/substrate/mesh/teams"
	"github.com/hollis-labs/tether/internal/teamhost"
	"github.com/hollis-labs/tether/internal/teamsvc"
	"gopkg.in/yaml.v3"
)

const (
	// FileName is resolved against the catalog root.
	FileName = "teams-policy.yaml"
	// PauseFileName is a marker beside the policy file. While it exists
	// formation is refused; removing it resumes formation. No restart needed.
	PauseFileName = "teams-policy.paused"

	Version = 1

	SlotCoordinator = "coordinator"
	SlotWorkers     = "workers"
	SlotReviewer    = "reviewer"

	maxWorkers = 2
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Launch binds one exact catalog launch id to one verified definition pin and
// the runtime family it executes on (used for the reviewer pairing rule).
type Launch struct {
	Launch     string `yaml:"launch"`
	Runtime    string `yaml:"runtime"`
	Definition Pin    `yaml:"definition"`
	// Source is the optional catalog-relative definition file ("catalog:<path>").
	// When set the daemon indexes it at start and refuses a pin mismatch.
	Source string `yaml:"source,omitempty"`
}
type Pin struct {
	ID       string `yaml:"id"`
	Revision string `yaml:"revision"`
	Digest   string `yaml:"digest"`
}

func (p Pin) Ref() mesh.DefinitionRef {
	return mesh.DefinitionRef{ID: p.ID, Revision: p.Revision, Digest: p.Digest}
}

type file struct {
	Version int `yaml:"version"`
	// SessionFormation lets a verified session principal form teams under the
	// same ceilings and allowlist; the session becomes the run owner. Default off.
	SessionFormation bool     `yaml:"session_formation"`
	Launches         []Launch `yaml:"launches"`
}

// Policy is immutable after Load. The zero value and nil both deny.
type Policy struct {
	launches    []Launch
	byPin       map[mesh.DefinitionRef]Launch
	pauseMarker string
	paused      atomic.Bool
	sessionForm bool
}

// SessionFormation reports whether verified sessions may form teams.
func (p *Policy) SessionFormation() bool { return p != nil && p.sessionForm }

// LaunchIDs lists the exact launch ids the policy binds.
func (p *Policy) LaunchIDs() []string {
	if p == nil {
		return nil
	}
	ids := make([]string, 0, len(p.launches))
	for _, l := range p.launches {
		ids = append(ids, l.Launch)
	}
	return ids
}

// Path returns where the policy file lives under the catalog root.
func Path(catalogRoot string) string { return filepath.Join(catalogRoot, FileName) }

// Load reads the policy from catalogRoot. An absent file yields (nil, nil):
// the caller treats nil as deny-all. A present but invalid file is an error
// naming the file and the offending entry; the daemon must not start teams.
func Load(catalogRoot string) (*Policy, error) {
	path := Path(catalogRoot)
	raw, err := os.ReadFile(path) //nolint:gosec // operator-owned file under the catalog root
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("teams policy %s: %w", path, err)
	}
	p, err := Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("teams policy %s: %w", path, err)
	}
	p.pauseMarker = filepath.Join(catalogRoot, PauseFileName)
	return p, nil
}

// Parse validates policy content. Unknown fields are errors.
func Parse(raw []byte) (*Policy, error) {
	var f file
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("invalid yaml: %w", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid yaml: exactly one document is allowed")
	}
	if f.Version != Version {
		return nil, fmt.Errorf("version must be %d, got %d", Version, f.Version)
	}
	if len(f.Launches) == 0 {
		return nil, errors.New("launches: at least one exact launch is required")
	}
	p := &Policy{byPin: map[mesh.DefinitionRef]Launch{}, sessionForm: f.SessionFormation}
	seen := map[string]bool{}
	for i, l := range f.Launches {
		at := fmt.Sprintf("launches[%d] (%q)", i, l.Launch)
		switch {
		case !idPattern.MatchString(l.Launch):
			return nil, fmt.Errorf("%s: launch must be an exact launch id (letters, digits, . _ -; no patterns)", at)
		case !idPattern.MatchString(l.Runtime):
			return nil, fmt.Errorf("%s: runtime is required (for example claude, codex, antigravity)", at)
		case l.Definition.ID == "" || l.Definition.Revision == "" || l.Definition.Digest == "":
			return nil, fmt.Errorf("%s: definition id, revision and digest are all required (exact pin)", at)
		case seen[l.Launch]:
			return nil, fmt.Errorf("%s: duplicate launch", at)
		}
		ref := l.Definition.Ref()
		if _, dup := p.byPin[ref]; dup {
			return nil, fmt.Errorf("%s: definition pin already bound to another launch", at)
		}
		seen[l.Launch] = true
		p.byPin[ref] = l
		p.launches = append(p.launches, l)
	}
	return p, nil
}

// Pause and Resume toggle the in-memory gate; the marker file is the
// restart-surviving, operator-visible equivalent.
func (p *Policy) Pause() {
	if p != nil {
		p.paused.Store(true)
	}
}
func (p *Policy) Resume() {
	if p != nil {
		p.paused.Store(false)
	}
}
func (p *Policy) Paused() bool {
	if p == nil {
		return true
	}
	if p.paused.Load() {
		return true
	}
	if p.pauseMarker != "" {
		// Only a confirmed absence resumes; any other stat failure fails closed.
		if _, err := os.Stat(p.pauseMarker); !errors.Is(err, os.ErrNotExist) {
			return true
		}
	}
	return false
}

// Launches reports the allowlist in file order.
func (p *Policy) Launches() []Launch {
	if p == nil {
		return nil
	}
	return slices.Clone(p.launches)
}

// LaunchTargets maps every pinned definition to its exact launch id.
func (p *Policy) LaunchTargets() map[mesh.DefinitionRef]string {
	out := map[mesh.DefinitionRef]string{}
	if p != nil {
		for ref, l := range p.byPin {
			out[ref] = l.Launch
		}
	}
	return out
}

// HostOptions are the trust tiers the team host needs: every allowlisted pin
// is trusted and the local operator is the only trusted actor.
func (p *Policy) HostOptions() teamhost.Options {
	opts := teamhost.Options{TrustTiers: map[mesh.DefinitionRef]teamhost.TrustTier{}, ActorTrust: map[mesh.URN]teamhost.TrustTier{}}
	if p == nil {
		return opts
	}
	for ref := range p.byPin {
		opts.TrustTiers[ref] = teamhost.TrustTrusted
	}
	opts.ActorTrust[teamsvc.LocalOperator] = teamhost.TrustTrusted
	if p.sessionForm {
		opts.OwnerAgentTrust = teamhost.TrustTrusted
	}
	return opts
}

// KnownCapability is the definition-policy capability vocabulary. Only the
// mesh spawn capability exists today; anything else is unknown and rejected.
func KnownCapability(uri string) bool { return uri == mesh.SpawnCapabilityURI }

// Ceilings is the teamsvc.Ceilings (and FormationAdmitter) implementation.
type Ceilings struct{ Policy *Policy }

var _ teamsvc.Ceilings = Ceilings{}
var _ teamsvc.FormationAdmitter = Ceilings{}

// StageA is the fixed Stage A formation ceiling: conservative limits at depth 1,
// messaging authority (message, assign, delegate) and at most three slots.
func StageA() teamsvc.FormationPolicy {
	f := teamsvc.ConservativePolicy()
	f.Limits.MaxDepth = 1
	f.MaxSlots = 3
	f.AllowedPermissions = []teams.Permission{teams.MayMessage, teams.MayAssign, teams.MayDelegate}
	return f
}

func (c Ceilings) FormationCeilings(_ context.Context, p teamsvc.Principal) (teamsvc.FormationPolicy, error) {
	if err := c.gate(p); err != nil {
		return teamsvc.FormationPolicy{}, err
	}
	return StageA(), nil
}

func (c Ceilings) gate(p teamsvc.Principal) error {
	if c.Policy == nil {
		return fmt.Errorf("%w: no teams policy provisioned", teamsvc.ErrDenied)
	}
	operator := p.Verified && p.LocalOperator && p.ID == teamsvc.LocalOperator && p.Kind == mesh.ActorUser
	session := c.Policy.sessionForm && p.Verified && !p.LocalOperator && p.Kind == mesh.ActorAgent && p.ID.Validate() == nil
	if !operator && !session {
		return fmt.Errorf("%w: team formation is operator-only", teamsvc.ErrDenied)
	}
	if c.Policy.Paused() {
		return fmt.Errorf("%w: team formation is paused", teamsvc.ErrUnavailable)
	}
	return nil
}

// AdmitFormation enforces the Stage A shape and runtime pairing. It runs
// before any definition write, so a refusal has no side effects.
func (c Ceilings) AdmitFormation(_ context.Context, p teamsvc.Principal, req teamsvc.FormRequest) error {
	if err := c.gate(p); err != nil {
		return err
	}
	deny := func(format string, a ...any) error {
		return fmt.Errorf("%w: "+format, append([]any{teamsvc.ErrDenied}, a...)...)
	}
	slots := map[string]teams.Slot{}
	for _, s := range req.Team.Slots {
		slots[s.Name] = s
	}
	if len(req.Team.Slots) != 3 || len(slots) != 3 {
		return deny("stage A teams have exactly three slots: %s, %s, %s", SlotCoordinator, SlotWorkers, SlotReviewer)
	}
	// The operator holds the coordinator slot as the run owner (a governance
	// seat, no session), so the slot provisions no member of its own: the
	// library gives a Fresh slot at most one member and the owner takes it.
	// Workers may be one fresh member or a pool of up to two pre-enrolled
	// agents (concurrent activation requires a pool).
	limits := map[string]struct{ min, max int }{SlotCoordinator: {0, 1}, SlotWorkers: {1, maxWorkers}, SlotReviewer: {1, 1}}
	runtimes := map[string]string{}
	for name, bounds := range limits {
		s, ok := slots[name]
		if !ok {
			return deny("missing slot %q", name)
		}
		if s.Resolution != teams.Fresh && (name != SlotWorkers || s.Resolution != teams.Pool) {
			return deny("slot %q must use fresh members (workers may use a pool)", name)
		}
		if s.Min < bounds.min || s.Max > bounds.max || s.Min > s.Max {
			return deny("slot %q size must be within %d..%d", name, bounds.min, bounds.max)
		}
		l, ok := c.Policy.byPin[s.Definition]
		if !ok {
			return deny("slot %q definition is not an allowlisted exact pin", name)
		}
		runtimes[name] = l.Runtime
	}
	// The operator's owner seat must be the coordinator slot, and that slot
	// must provision no member, so an agent can never take the coordinator role.
	owner := req.Team.Routing.CoordinatorSlot
	if len(req.Team.Phases) > 0 && req.Team.Phases[0].OwnerSlot != "" {
		owner = req.Team.Phases[0].OwnerSlot
	}
	if req.Team.Routing.CoordinatorSlot != SlotCoordinator || owner != SlotCoordinator {
		return deny("coordinator and owner slot must both be %q", SlotCoordinator)
	}
	if slots[SlotCoordinator].Min != 0 || req.Launch.Counts[SlotCoordinator] != 0 {
		return deny("the coordinator slot is the operator's seat and provisions no member")
	}
	// Reviewer must be on a different runtime from the workers when the
	// allowlist offers more than one runtime; with a single runtime it is
	// only preferred.
	distinct := map[string]bool{}
	for _, l := range c.Policy.launches {
		distinct[l.Runtime] = true
	}
	if len(distinct) > 1 && runtimes[SlotReviewer] == runtimes[SlotWorkers] {
		return deny("reviewer must run on a different runtime than the workers (%s)", runtimes[SlotWorkers])
	}
	return nil
}
