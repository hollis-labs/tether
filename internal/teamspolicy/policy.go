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
	"log"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hollis-labs/libs/mesh/core"
	"github.com/hollis-labs/libs/mesh/core/teams"
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
	// SlotOrchestrator is the Stage B agent that runs the team: it is the only
	// member workers and the reviewer may reach, and its reported result ends
	// the run. The coordinator slot stays the owner's governance seat.
	SlotOrchestrator = "orchestrator"

	maxWorkers = 2
	// maxWorkersB and maxAgentsB bound a Stage B team: one orchestrator, one to
	// three workers and one reviewer, never more than five agents in all.
	maxWorkersB = 3
	maxAgentsB  = 5

	// Reviewer pairing modes (reviewer_pairing in the policy file).
	PairingRequire = "require"
	PairingPrefer  = "prefer"
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Launch binds one exact catalog launch id to one verified definition pin and
// the runtime family it executes on (used for the reviewer pairing rule).
type Launch struct {
	Launch  string `yaml:"launch"`
	Runtime string `yaml:"runtime"`
	// Model is optional. When both reviewer and worker launches set it, a
	// different model on the same runtime satisfies the reviewer pairing rule.
	Model      string `yaml:"model,omitempty"`
	Definition Pin    `yaml:"definition"`
	// Source is the optional catalog-relative definition file ("catalog:<path>").
	// When set the daemon indexes it at start and refuses a pin mismatch.
	Source string `yaml:"source,omitempty"`
}
type Pin struct {
	ID       string `yaml:"id" json:"id"`
	Revision string `yaml:"revision" json:"revision"`
	Digest   string `yaml:"digest" json:"digest"`
}

func (p Pin) Ref() mesh.DefinitionRef {
	return mesh.DefinitionRef{ID: p.ID, Revision: p.Revision, Digest: p.Digest}
}

type file struct {
	Version int `yaml:"version"`
	// SessionFormation lets a verified session principal form teams under the
	// same ceilings and allowlist; the session becomes the run owner. Default off.
	SessionFormation bool `yaml:"session_formation"`
	// Adoption lets the run owner adopt already-running fleet agents into pool
	// seats (see docs/teams-activation.md). Default off; read at daemon start.
	Adoption bool `yaml:"adoption"`
	// Services lists the service principals (for example Torque's dispatcher)
	// that may form and dissolve teams, in addition to the local operator. A
	// service must still present a valid credential; an unlisted service is
	// refused. Default: none.
	Services []string `yaml:"services"`
	// Ceilings bounds and defaults the spawn limits of a formation; omitted
	// fields take the built-in Stage B values.
	Ceilings *ceilingsFile `yaml:"ceilings"`
	// ReviewerPairing is "require" (default) or "prefer".
	ReviewerPairing string   `yaml:"reviewer_pairing"`
	Launches        []Launch `yaml:"launches"`
}

// LimitSpec is the YAML form of mesh.Limits. Zero fields mean "built-in".
type LimitSpec struct {
	Timeout     time.Duration `yaml:"timeout"`
	MaxRounds   int           `yaml:"max_rounds"`
	MaxStalls   int           `yaml:"max_stalls"`
	MaxDepth    int           `yaml:"max_depth"`
	MaxChildren int           `yaml:"max_children"`
	FanOut      int           `yaml:"fan_out"`
	Budget      float64       `yaml:"budget"`
}
type ceilingsFile struct {
	Default LimitSpec `yaml:"default"`
	Max     LimitSpec `yaml:"max"`
}

// Built-in Stage B ceilings. The maximum is a hard cap no team or launch
// request may widen; the default applies to whatever a request leaves unset.
// They replace the Stage A conservative limits (five minutes, budget 1, 16
// rounds, depth 1) with room for real multi-hour work.
var (
	builtinMax     = mesh.Limits{Timeout: 12 * time.Hour, MaxRounds: 500, MaxStalls: 40, MaxDepth: 3, MaxChildren: 8, FanOut: 8, Budget: 10000}
	builtinDefault = mesh.Limits{Timeout: 4 * time.Hour, MaxRounds: 200, MaxStalls: 20, MaxDepth: 2, MaxChildren: 6, FanOut: 6, Budget: 2000}
)

// Policy is immutable after Load. The zero value and nil both deny.
type Policy struct {
	launches    []Launch
	byPin       map[mesh.DefinitionRef]Launch
	pauseMarker string
	paused      atomic.Bool
	sessionForm bool
	adoption    bool
	services    map[mesh.URN]bool
	maxLimits   mesh.Limits
	defLimits   mesh.Limits
	pairing     string
	pairingMu   sync.Mutex
	pairings    map[string]ReviewerPairing
}

// SessionFormation reports whether verified sessions may form teams.
func (p *Policy) SessionFormation() bool { return p != nil && p.sessionForm }

// Adoption reports whether adopting running agents into a team is enabled.
func (p *Policy) Adoption() bool { return p != nil && p.adoption }

// IsService reports whether id is a policy-listed service principal that may
// form and dissolve teams.
func (p *Policy) IsService(id string) bool { return p != nil && p.services[mesh.URN(id)] }

// Services lists the policy-listed service principals.
func (p *Policy) Services() []string {
	if p == nil {
		return nil
	}
	out := make([]string, 0, len(p.services))
	for id := range p.services {
		out = append(out, string(id))
	}
	slices.Sort(out)
	return out
}

// MaxLimits is the hard formation ceiling; DefaultLimits fills what a request
// leaves unspecified. Both are the built-in Stage B values for a nil policy.
func (p *Policy) MaxLimits() mesh.Limits {
	if p == nil {
		return builtinMax
	}
	return p.maxLimits
}
func (p *Policy) DefaultLimits() mesh.Limits {
	if p == nil {
		return builtinDefault
	}
	return p.defLimits
}

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
		if strings.Contains(err.Error(), "time.Duration") {
			return nil, fmt.Errorf("invalid yaml: %w (a timeout must be a duration string such as 4h, not a bare number)", err)
		}
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
	p := &Policy{byPin: map[mesh.DefinitionRef]Launch{}, sessionForm: f.SessionFormation, adoption: f.Adoption, services: map[mesh.URN]bool{}}
	for i, id := range f.Services {
		urn := mesh.URN(id)
		if (mesh.Actor{URN: urn, Kind: mesh.ActorService}).Validate() != nil {
			return nil, fmt.Errorf("services[%d] (%q): must be a service principal urn like msg://service/torque/dispatcher", i, id)
		}
		if p.services[urn] {
			return nil, fmt.Errorf("services[%d] (%q): duplicate service", i, id)
		}
		p.services[urn] = true
	}
	var err error
	if p.maxLimits, p.defLimits, err = resolveCeilings(f.Ceilings); err != nil {
		return nil, fmt.Errorf("ceilings: %w", err)
	}
	switch f.ReviewerPairing {
	case "", PairingRequire:
		p.pairing = PairingRequire
	case PairingPrefer:
		p.pairing = PairingPrefer
	default:
		return nil, fmt.Errorf("reviewer_pairing must be %q or %q, got %q", PairingRequire, PairingPrefer, f.ReviewerPairing)
	}
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

func mergeLimits(spec LimitSpec, base mesh.Limits) mesh.Limits {
	out := base
	if spec.Timeout != 0 {
		out.Timeout = spec.Timeout
	}
	if spec.MaxRounds != 0 {
		out.MaxRounds = spec.MaxRounds
	}
	if spec.MaxStalls != 0 {
		out.MaxStalls = spec.MaxStalls
	}
	if spec.MaxDepth != 0 {
		out.MaxDepth = spec.MaxDepth
	}
	if spec.MaxChildren != 0 {
		out.MaxChildren = spec.MaxChildren
	}
	if spec.FanOut != 0 {
		out.FanOut = spec.FanOut
	}
	if spec.Budget != 0 {
		out.Budget = spec.Budget
	}
	return out
}

// resolveCeilings applies the file over the built-ins. A default the file does
// not set is clamped to the maximum; one it sets above the maximum is an error.
func resolveCeilings(c *ceilingsFile) (maxL, defL mesh.Limits, err error) {
	var spec ceilingsFile
	if c != nil {
		spec = *c
	}
	for name, d := range map[string]time.Duration{"default": spec.Default.Timeout, "max": spec.Max.Timeout} {
		if d != 0 && d < time.Second {
			return maxL, defL, fmt.Errorf("%s timeout %s is below 1s (a bare number is read as nanoseconds): use a duration such as 4h", name, d)
		}
	}
	maxL = mergeLimits(spec.Max, builtinMax)
	defL = mergeLimits(spec.Default, builtinDefault)
	if err = maxL.Validate(); err != nil {
		return maxL, defL, fmt.Errorf("max: %w", err)
	}
	if maxL.MaxRounds < 1 || maxL.MaxStalls < 1 {
		return maxL, defL, errors.New("max: max_rounds and max_stalls must be positive")
	}
	if err = defL.Validate(); err != nil {
		return maxL, defL, fmt.Errorf("default: %w", err)
	}
	type pair struct {
		name     string
		def, max float64
		set      bool
		clamp    func()
	}
	for _, f := range []pair{
		{"timeout", float64(defL.Timeout), float64(maxL.Timeout), spec.Default.Timeout != 0, func() { defL.Timeout = maxL.Timeout }},
		{"max_rounds", float64(defL.MaxRounds), float64(maxL.MaxRounds), spec.Default.MaxRounds != 0, func() { defL.MaxRounds = maxL.MaxRounds }},
		{"max_stalls", float64(defL.MaxStalls), float64(maxL.MaxStalls), spec.Default.MaxStalls != 0, func() { defL.MaxStalls = maxL.MaxStalls }},
		{"max_depth", float64(defL.MaxDepth), float64(maxL.MaxDepth), spec.Default.MaxDepth != 0, func() { defL.MaxDepth = maxL.MaxDepth }},
		{"max_children", float64(defL.MaxChildren), float64(maxL.MaxChildren), spec.Default.MaxChildren != 0, func() { defL.MaxChildren = maxL.MaxChildren }},
		{"fan_out", float64(defL.FanOut), float64(maxL.FanOut), spec.Default.FanOut != 0, func() { defL.FanOut = maxL.FanOut }},
		{"budget", defL.Budget, maxL.Budget, spec.Default.Budget != 0, func() { defL.Budget = maxL.Budget }},
	} {
		if f.def <= f.max {
			continue
		}
		if f.set {
			return maxL, defL, fmt.Errorf("default %s exceeds max %s", f.name, f.name)
		}
		f.clamp()
	}
	return maxL, defL, nil
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
	for id := range p.services {
		opts.ActorTrust[id] = teamhost.TrustTrusted
	}
	if p.sessionForm {
		opts.OwnerAgentTrust = teamhost.TrustTrusted
	}
	if p.adoption {
		// A session may be adopted only if it runs an allowlisted launch with
		// exactly that launch's pin; the host reads both from the launch record.
		opts.AdoptableLaunches = teamhost.AdoptedLaunches{}
		for _, l := range p.launches {
			opts.AdoptableLaunches[l.Launch] = l.Definition.Ref()
		}
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
var _ teamsvc.AdoptionAdmitter = Ceilings{}

// StageA is the Stage A formation shape with its original conservative limits
// (five minutes, budget 1, 16 rounds, depth 1). It is kept for callers that
// want those tight limits in a team's own spawn policy; the daemon's ceilings
// are the policy's Stage B limits (see FormationCeilings).
func StageA() teamsvc.FormationPolicy {
	f := teamsvc.ConservativePolicy()
	f.Limits.MaxDepth = 1
	f.MaxSlots = 3
	f.AllowedPermissions = []teams.Permission{teams.MayMessage, teams.MayAssign, teams.MayDelegate}
	return f
}

// StageB is the formation shape ceiling: up to four slots (the owner seat plus
// orchestrator, workers, reviewer), at most six initial members (five agents
// and the owner) and the policy's limits. Messaging authority only.
func (p *Policy) StageB() teamsvc.FormationPolicy {
	return teamsvc.FormationPolicy{
		Limits: p.MaxLimits(), DefaultLimits: p.DefaultLimits(),
		MaxSlots: 4, MaxMembersPerSlot: maxWorkersB, MaxInitialMembers: maxAgentsB + 1,
		AllowedPermissions: []teams.Permission{teams.MayMessage, teams.MayAssign, teams.MayDelegate},
	}
}

// FormationCeilings are the Stage A ceilings (the conservative limits). A
// Stage B request gets the policy's limits through FormationCeilingsFor.
func (c Ceilings) FormationCeilings(_ context.Context, p teamsvc.Principal) (teamsvc.FormationPolicy, error) {
	if err := c.gate(p); err != nil {
		return teamsvc.FormationPolicy{}, err
	}
	f := StageA()
	f.AllowAdoptedSlots = c.Policy.adoption
	return f, nil
}

var _ teamsvc.ShapedCeilings = Ceilings{}

// isStageB reports whether the team uses the orchestrator slot.
func isStageB(t teams.Team) bool {
	return slices.ContainsFunc(t.Slots, func(s teams.Slot) bool { return s.Name == SlotOrchestrator })
}

// FormationCeilingsFor gives a Stage B team (one with an orchestrator slot) the
// policy's Stage B ceilings and every other team the Stage A ceilings.
func (c Ceilings) FormationCeilingsFor(ctx context.Context, p teamsvc.Principal, req teamsvc.FormRequest) (teamsvc.FormationPolicy, error) {
	f, err := c.FormationCeilings(ctx, p)
	if err != nil || !isStageB(req.Team) {
		return f, err
	}
	f = c.Policy.StageB()
	f.AllowAdoptedSlots = c.Policy.adoption
	return f, nil
}

// AdmitAdoption refuses adoption unless the policy enables it and the gate
// (operator, listed service or permitted session, not paused) admits the
// caller. Everything about the actor and session is verified by the host.
func (c Ceilings) AdmitAdoption(_ context.Context, p teamsvc.Principal, _ teams.Team, _ teams.Slot) error {
	if err := c.gate(p); err != nil {
		return err
	}
	if !c.Policy.adoption {
		return fmt.Errorf("%w: adoption is not enabled in the teams policy", teamsvc.ErrDenied)
	}
	return nil
}

// DissolvesOnResult makes the orchestrator's reported result the final one:
// the run dissolves and every member session stops. Only Stage B teams (those
// with an orchestrator slot) end this way.
func (c Ceilings) DissolvesOnResult(t teams.Team, reporterSlot string) bool {
	return c.Policy != nil && reporterSlot == SlotOrchestrator && isStageB(t)
}

// AdmitMemberAdd limits what may be added to a Stage B run: only workers, at
// most three of them, and never more than five agents in all. Counted members
// are those still holding a seat (live or terminating); the owner is not an
// agent. The added worker has the workers slot's grants, so it can message only
// the orchestrator.
func (c Ceilings) AdmitMemberAdd(t teams.Team, roster teams.Roster, slot string) error {
	if c.Policy == nil || !isStageB(t) {
		return nil
	}
	if slot != SlotWorkers {
		return deny("only %s may be added to a stage B run", SlotWorkers)
	}
	agents, workers := 0, 0
	for _, m := range roster.Members {
		if m.Governance == teams.Owner {
			continue
		}
		switch m.Status {
		case "active", "provisioning", "stopping", "releasing", "failing":
			agents++
			if m.Slot == SlotWorkers {
				workers++
			}
		}
	}
	if workers >= maxWorkersB {
		return deny("a stage B run has at most %d workers", maxWorkersB)
	}
	if agents >= maxAgentsB {
		return deny("a team has at most %d agents", maxAgentsB)
	}
	return nil
}

var _ teamsvc.MemberAddPolicy = Ceilings{}

// ManagesLifecycle reports that only Stage B runs are reaped (deadline, all stopped).
func (c Ceilings) ManagesLifecycle(t teams.Team) bool { return c.Policy != nil && isStageB(t) }

var _ teamsvc.LifecyclePolicy = Ceilings{}

func (c Ceilings) gate(p teamsvc.Principal) error {
	if c.Policy == nil {
		return fmt.Errorf("%w: no teams policy provisioned", teamsvc.ErrDenied)
	}
	operator := p.Verified && p.LocalOperator && p.ID == teamsvc.LocalOperator && p.Kind == mesh.ActorUser
	service := p.Verified && !p.LocalOperator && p.Kind == mesh.ActorService && c.Policy.IsService(string(p.ID))
	session := c.Policy.sessionForm && p.Verified && !p.LocalOperator && p.Kind == mesh.ActorAgent && p.ID.Validate() == nil
	if !operator && !service && !session {
		return fmt.Errorf("%w: team formation is limited to the operator and policy-listed services", teamsvc.ErrDenied)
	}
	if c.Policy.Paused() {
		return fmt.Errorf("%w: team formation is paused", teamsvc.ErrUnavailable)
	}
	return nil
}

func deny(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{teamsvc.ErrDenied}, a...)...)
}

// AdmitFormation enforces the Stage A or Stage B shape and runtime pairing. It
// runs before any definition write, so a refusal has no side effects. A team
// with an orchestrator slot is Stage B; anything else must be Stage A shaped.
func (c Ceilings) AdmitFormation(_ context.Context, p teamsvc.Principal, req teamsvc.FormRequest) error {
	if err := c.gate(p); err != nil {
		return err
	}
	slots := map[string]teams.Slot{}
	for _, s := range req.Team.Slots {
		slots[s.Name] = s
	}
	if _, ok := slots[SlotOrchestrator]; ok {
		return c.admitStageB(req, slots)
	}
	return c.admitStageA(req, slots)
}

// admitOwner checks the owner seat: the coordinator slot is held by the forming
// principal as run owner (a governance seat, no session), so it provisions no
// member of its own and an agent can never take it.
func admitOwner(req teamsvc.FormRequest, slots map[string]teams.Slot) error {
	owner := req.Team.Routing.CoordinatorSlot
	if len(req.Team.Phases) > 0 && req.Team.Phases[0].OwnerSlot != "" {
		owner = req.Team.Phases[0].OwnerSlot
	}
	if req.Team.Routing.CoordinatorSlot != SlotCoordinator || owner != SlotCoordinator {
		return deny("coordinator and owner slot must both be %q", SlotCoordinator)
	}
	if slots[SlotCoordinator].Min != 0 || req.Launch.Counts[SlotCoordinator] != 0 {
		return deny("the coordinator slot is the owner's seat and provisions no member")
	}
	return nil
}

func (c Ceilings) admitStageA(req teamsvc.FormRequest, slots map[string]teams.Slot) error {
	if len(req.Team.Slots) != 3 || len(slots) != 3 {
		return deny("stage A teams have exactly three slots: %s, %s, %s (or add %q for a stage B team)", SlotCoordinator, SlotWorkers, SlotReviewer, SlotOrchestrator)
	}
	// The operator holds the coordinator slot as the run owner (a governance
	// seat, no session), so the slot provisions no member of its own: the
	// library gives a Fresh slot at most one member and the owner takes it.
	// Workers may be one fresh member or a pool of up to two pre-enrolled
	// agents (concurrent activation requires a pool).
	limits := map[string]struct{ min, max int }{SlotCoordinator: {0, 1}, SlotWorkers: {1, maxWorkers}, SlotReviewer: {1, 1}}
	if c.Policy.adoption {
		// A pool seat is filled by adoption: it provisions nothing, so it may
		// start empty. The coordinator pool holds the owner's seat plus one
		// delegate; workers and reviewer keep their maxima.
		limits = map[string]struct{ min, max int }{SlotCoordinator: {0, 1}, SlotWorkers: {0, maxWorkers}, SlotReviewer: {0, 1}}
	}
	launches := map[string]Launch{}
	for name, bounds := range limits {
		s, ok := slots[name]
		if !ok {
			return deny("missing slot %q", name)
		}
		adoptable := c.Policy.adoption && s.Resolution == teams.Pool
		if s.Resolution != teams.Fresh && (name != SlotWorkers || s.Resolution != teams.Pool) && !adoptable {
			return deny("slot %q must use fresh members (workers may use a pool)", name)
		}
		if adoptable && s.Min != 0 {
			return deny("adoptable pool slot %q must start empty (min 0)", name)
		}
		if adoptable && name == SlotCoordinator {
			// The operator keeps the owner seat; one delegate may share the slot.
			bounds.max = 2
		}
		if s.Min < bounds.min || s.Max > bounds.max || s.Min > s.Max {
			return deny("slot %q size must be within %d..%d", name, bounds.min, bounds.max)
		}
		l, ok := c.Policy.byPin[s.Definition]
		if !ok {
			return deny("slot %q definition is not an allowlisted exact pin", name)
		}
		launches[name] = l
	}
	if err := admitOwner(req, slots); err != nil {
		return err
	}
	return c.admitStageAPairing(req.Key, launches[SlotWorkers], launches[SlotReviewer])
}

// stageBGrants is every grant a Stage B team may carry, and nothing else.
// The orchestrator directs workers and the reviewer; workers and the reviewer
// may reach only the orchestrator, never each other; the owner's seat reaches
// only the orchestrator (it hands the task over and receives the result).
var stageBGrants = func() map[teams.Grant]bool {
	out := map[teams.Grant]bool{}
	for _, v := range []teams.Permission{teams.MayMessage, teams.MayAssign, teams.MayDelegate} {
		out[teams.Grant{FromSlot: SlotCoordinator, Verb: v, ToSlot: SlotOrchestrator}] = true
		out[teams.Grant{FromSlot: SlotOrchestrator, Verb: v, ToSlot: SlotWorkers}] = true
		out[teams.Grant{FromSlot: SlotOrchestrator, Verb: v, ToSlot: SlotReviewer}] = true
	}
	out[teams.Grant{FromSlot: SlotWorkers, Verb: teams.MayMessage, ToSlot: SlotOrchestrator}] = true
	out[teams.Grant{FromSlot: SlotReviewer, Verb: teams.MayMessage, ToSlot: SlotOrchestrator}] = true
	return out
}()

// admitStageB enforces the orchestrator-led shape: slots coordinator (owner
// seat), orchestrator (1), workers (1..3) and reviewer (1), so a team is three
// to five agents and never more than five, with the fixed authority above.
func (c Ceilings) admitStageB(req teamsvc.FormRequest, slots map[string]teams.Slot) error {
	if len(req.Team.Slots) != 4 || len(slots) != 4 {
		return deny("stage B teams have exactly four slots: %s, %s, %s, %s", SlotCoordinator, SlotOrchestrator, SlotWorkers, SlotReviewer)
	}
	type bound struct{ min, max int }
	shape := map[string]bound{SlotCoordinator: {0, 1}, SlotOrchestrator: {1, 1}, SlotWorkers: {1, maxWorkersB}, SlotReviewer: {1, 1}}
	launches := map[string]Launch{}
	count := map[string]int{}
	for name, b := range shape {
		s, ok := slots[name]
		if !ok {
			return deny("missing slot %q", name)
		}
		// Only workers may be a pool (needed for more than one concurrent
		// member); every other seat is a fresh single member.
		if s.Resolution != teams.Fresh && (name != SlotWorkers || s.Resolution != teams.Pool) {
			return deny("slot %q must use fresh members (workers may use a pool)", name)
		}
		if s.Min < b.min || s.Max > b.max || s.Min > s.Max {
			return deny("slot %q size must be within %d..%d", name, b.min, b.max)
		}
		l, ok := c.Policy.byPin[s.Definition]
		if !ok {
			return deny("slot %q definition is not an allowlisted exact pin", name)
		}
		launches[name] = l
		count[name] = s.Min
		if n, ok := req.Launch.Counts[name]; ok {
			count[name] = n
		}
	}
	if err := admitOwner(req, slots); err != nil {
		return err
	}
	if count[SlotOrchestrator] != 1 || count[SlotReviewer] != 1 || count[SlotWorkers] < 1 || count[SlotWorkers] > maxWorkersB {
		return deny("a stage B team launches one %s, one to %d %s and one %s", SlotOrchestrator, maxWorkersB, SlotWorkers, SlotReviewer)
	}
	if agents := count[SlotOrchestrator] + count[SlotWorkers] + count[SlotReviewer]; agents > maxAgentsB {
		return deny("a team has at most %d agents, got %d", maxAgentsB, agents)
	}
	for _, g := range req.Team.Authority.Grants {
		if !stageBGrants[g] {
			return deny("grant %s %s -> %s is not allowed in a stage B team (workers and the reviewer may message only the %s)", g.FromSlot, g.Verb, g.ToSlot, SlotOrchestrator)
		}
	}
	return c.admitPairing(req.Key, launches[SlotWorkers], launches[SlotReviewer])
}

// admitStageAPairing is the Stage A reviewer rule, unchanged: a different
// runtime (or model when both name one) whenever more than one runtime is
// allowlisted, otherwise only a preference.
func (c Ceilings) admitStageAPairing(key string, workers, reviewer Launch) error {
	pairing := ReviewerPairing{WorkerRuntime: workers.Runtime, WorkerModel: workers.Model, ReviewerRuntime: reviewer.Runtime, ReviewerModel: reviewer.Model}
	pairing.Different = launchesDiffer(workers, reviewer)
	if !pairing.Different {
		distinct := map[string]bool{}
		for _, l := range c.Policy.launches {
			distinct[l.Runtime] = true
		}
		if len(distinct) > 1 {
			return deny("reviewer must run on a different runtime or model than the workers (%s)", pairing.WorkerRuntime)
		}
		log.Printf("teams: reviewer pairing fallback for run %q: only runtime %q is allowlisted, reviewer shares runtime/model with workers", key, pairing.WorkerRuntime)
	}
	c.Policy.recordPairing(key, pairing)
	return nil
}

// admitPairing applies the Stage B reviewer rule: the reviewer runs on a different
// runtime or model than the workers. It is required when the allowlist can
// satisfy it and the policy says "require" (the default); when the allowlist
// has no two launches that differ, or the policy says "prefer", the formation
// proceeds and the fallback is logged and recorded for later evals.
func (c Ceilings) admitPairing(key string, workers, reviewer Launch) error {
	pairing := ReviewerPairing{WorkerRuntime: workers.Runtime, WorkerModel: workers.Model, ReviewerRuntime: reviewer.Runtime, ReviewerModel: reviewer.Model}
	pairing.Different = launchesDiffer(workers, reviewer)
	if !pairing.Different {
		if c.Policy.pairing != PairingPrefer && c.Policy.pairingSatisfiable() {
			return deny("reviewer must run on a different runtime or model than the workers (%s)", pairing.WorkerRuntime)
		}
		log.Printf("teams: reviewer pairing fallback for run %q: reviewer shares runtime/model (%s) with workers (allowlist cannot or need not separate them)", key, pairing.WorkerRuntime)
	}
	c.Policy.recordPairing(key, pairing)
	return nil
}

func launchesDiffer(a, b Launch) bool {
	return a.Runtime != b.Runtime || (a.Model != "" && b.Model != "" && a.Model != b.Model)
}

// pairingSatisfiable reports whether any two allowlisted launches differ by
// runtime or model, so a different reviewer is possible at all.
func (p *Policy) pairingSatisfiable() bool {
	for i, a := range p.launches {
		for _, b := range p.launches[i+1:] {
			if launchesDiffer(a, b) {
				return true
			}
		}
	}
	return false
}

// ReviewerPairing records the author (worker) and reviewer runtime/model chosen
// by an admitted formation, keyed by its request key; Different is false for the recorded
// single-runtime fallback.
type ReviewerPairing = teamsvc.ReviewerPairing

// maxPairings bounds the per-formation pairing map; it is cleared when full
// since a pairing is only read shortly after its formation is admitted.
const maxPairings = 1024

func (p *Policy) recordPairing(key string, r ReviewerPairing) {
	p.pairingMu.Lock()
	defer p.pairingMu.Unlock()
	if p.pairings == nil || len(p.pairings) >= maxPairings {
		p.pairings = map[string]ReviewerPairing{}
	}
	p.pairings[key] = r
}

// PairingFor returns the pairing recorded when the formation with request key
// was admitted, and false if none was (denied, unknown or evicted).
func (p *Policy) PairingFor(key string) (ReviewerPairing, bool) {
	p.pairingMu.Lock()
	defer p.pairingMu.Unlock()
	r, ok := p.pairings[key]
	return r, ok
}

// PairingFor lets the formation path read the pairing AdmitFormation recorded
// for the request key, so it can be emitted on the formation result.
func (c Ceilings) PairingFor(key string) (teamsvc.ReviewerPairing, bool) {
	if c.Policy == nil {
		return teamsvc.ReviewerPairing{}, false
	}
	return c.Policy.PairingFor(key)
}
