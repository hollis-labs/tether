package specresolve

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hollis-labs/go-agent-launch/agentlaunch"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launchresolve"
)

// DefaultSpecsRoot is the documented default location of the LaunchSpec
// corpus, relative to the user home directory. It mirrors the catalog
// convention internal/launchresolve uses (~/.tether/catalog) — the launch-spec
// corpus is a sibling directory under the same root.
//
// It is a DEFAULT, not a hardcode: NewResolver always accepts an explicit
// specsRoot via WithSpecsRoot, and the package's own tests point it at the
// in-repo testdata/launch-specs corpus. This package never reads or writes
// the live ~/.tether/catalog/.
const DefaultSpecsRoot = ".tether/launch-specs"

// AssemblyFileName is the corpus file holding the single canonical
// LaunchSpec. Every LaunchBag's `spec` ref resolves to this file (the S5
// corpus is one-spec-many-bags). If a future corpus splits the spec into
// <spec>.yaml-per-id files, resolveSpecPath is the one place to extend.
const AssemblyFileName = "launch-assembly.yaml"

// LaunchesDir is the corpus subdirectory holding the per-launch LaunchBag
// files. The bag filename stem is the launch id.
const LaunchesDir = "launches"

// Package-level sentinel errors. Each is errors.Is-comparable so callers
// branch precisely.
var (
	// ErrLaunchNotFound is returned by Resolve when no LaunchBag file
	// matches the launch id under <specsRoot>/launches/.
	ErrLaunchNotFound = errors.New("specresolve: launch not found")

	// ErrSpecNotFound is returned by Resolve when a bag's `spec` ref does
	// not resolve to a LaunchSpec file under the corpus root.
	ErrSpecNotFound = errors.New("specresolve: launch spec not found")

	// ErrMissingRequiredInputs is returned by Resolve when an autonomous
	// render reports unsatisfied required inputs (the S4.1 autonomous
	// contract: no human to collect them from).
	ErrMissingRequiredInputs = errors.New("specresolve: launch render missing required inputs")
)

// Resolver resolves a launch id to a runnable agentlaunch.LaunchPlan via
// the S5 Spec path. It is constructed once with NewResolver and is safe to
// reuse across launches: the trust authorizer and call resolver are
// stateless, and runner/agent resolution dispatches through the
// concurrency-safe *launchresolve.Registry.
type Resolver struct {
	// specsRoot is the absolute path of the LaunchSpec corpus directory
	// (containing launch-assembly.yaml and launches/).
	specsRoot string

	// reg resolves the bag's runner input to a RuntimeBinding and the
	// agent input to an AgentSpec.
	reg *launchresolve.Registry

	// authorizer gates call/cmd var sources (D6c).
	authorizer agentlaunch.TrustAuthorizer

	// callResolver executes the corpus's http `call` var sources.
	callResolver agentlaunch.CallResolver
}

// Option configures a Resolver at construction time.
type Option func(*Resolver)

// WithSpecsRoot sets the LaunchSpec corpus directory. The path is
// ~-expanded. When this option is not supplied, NewResolver falls back to
// <home>/.tether/launch-specs (DefaultSpecsRoot).
func WithSpecsRoot(root string) Option {
	return func(r *Resolver) {
		if root != "" {
			r.specsRoot = config.Expand(root)
		}
	}
}

// WithCallResolver overrides the http CallResolver used for the corpus's
// `call` var sources. It exists for tests (a stub recall endpoint) and for
// callers wanting a custom transport; production callers use the default.
func WithCallResolver(cr agentlaunch.CallResolver) Option {
	return func(r *Resolver) {
		if cr != nil {
			r.callResolver = cr
		}
	}
}

// WithTrustAuthorizer overrides the TrustAuthorizer used to gate call/cmd
// var sources. The default authorizes only the catalog-authored trust
// tokens (see CatalogTrustTokens); overriding it is a deliberate policy
// choice, mainly for tests.
func WithTrustAuthorizer(ta agentlaunch.TrustAuthorizer) Option {
	return func(r *Resolver) {
		if ta != nil {
			r.authorizer = ta
		}
	}
}

// NewResolver constructs a Resolver. reg is the registry the resolver uses
// to resolve runner and agent inputs and is required. specsRoot defaults
// to <home>/.tether/launch-specs and is overridable with WithSpecsRoot.
//
// Construction performs no I/O beyond resolving the home directory for the
// default specsRoot; the corpus files are read lazily by Resolve.
func NewResolver(reg *launchresolve.Registry, opts ...Option) (*Resolver, error) {
	if reg == nil {
		return nil, errors.New("specresolve: NewResolver requires a non-nil registry")
	}

	r := &Resolver{
		reg:          reg,
		authorizer:   catalogTrustAuthorizer{},
		callResolver: newHTTPCallResolver(),
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("specresolve: resolve home directory: %w", err)
	}
	r.specsRoot = filepath.Join(home, DefaultSpecsRoot)

	for _, opt := range opts {
		opt(r)
	}
	return r, nil
}

// SpecsRoot returns the absolute LaunchSpec corpus directory the resolver
// reads from.
func (r *Resolver) SpecsRoot() string { return r.specsRoot }

// Resolve resolves launchID to a runnable agentlaunch.LaunchPlan via the
// full S5 Spec pipeline:
//
//  1. load <specsRoot>/launches/<launchID>.yaml as a LaunchBag, and the
//     bag's `spec` ref as a LaunchSpec from the corpus;
//  2. ValidateMinimumConfig(spec, bag);
//  3. render the bag against the spec to resolve the input values, then
//     run S4.2 var resolution over the (input-expanded) var sources;
//  4. re-render with the resolved vars -> RenderResult;
//  5. resolve the bag's `runner` input -> RuntimeBinding and `agent`
//     input -> AgentSpec (both HARD errors when unresolvable);
//  6. assemble via agentlaunch.PlanFromLaunch.
//
// frontEnd selects missing-required-input handling: FrontEndAutonomous
// turns an unsatisfied required input into ErrMissingRequiredInputs;
// FrontEndInteractive surfaces it on the (unused) Missing list instead.
// The LaunchMode stamped on the plan is derived from frontEnd.
//
// The returned LaunchPlan is Validate()-clean (PlanFromLaunch validates it)
// and ready for the launcher.Compile -> Prepare -> Plant pipeline.
func (r *Resolver) Resolve(launchID string, frontEnd agentlaunch.RenderFrontEnd) (agentlaunch.LaunchPlan, error) {
	return r.ResolveContext(context.Background(), launchID, frontEnd)
}

// ResolveContext is Resolve with a caller-supplied context. The context
// bounds the var-resolution I/O (http calls and cmd execution); it is the
// seam a caller uses to cancel a slow recall endpoint.
func (r *Resolver) ResolveContext(ctx context.Context, launchID string, frontEnd agentlaunch.RenderFrontEnd) (agentlaunch.LaunchPlan, error) {
	if launchID == "" {
		return agentlaunch.LaunchPlan{}, fmt.Errorf("%w: empty launch id", ErrLaunchNotFound)
	}

	// 1. Load the bag and its spec.
	bag, err := r.loadBag(launchID)
	if err != nil {
		return agentlaunch.LaunchPlan{}, err
	}
	spec, err := r.loadSpec(bag.Spec)
	if err != nil {
		return agentlaunch.LaunchPlan{}, err
	}

	// 2. Cross-check the bag against the spec's minimum-config contract.
	if err := agentlaunch.ValidateMinimumConfig(spec, bag); err != nil {
		return agentlaunch.LaunchPlan{}, fmt.Errorf("specresolve: launch %q: %w", launchID, err)
	}

	// 3a. A first render yields the effective input map (defaults
	// applied). Var sources are parameterized by inputs, so the input map
	// must be known before var resolution. This render is always done
	// FrontEndInteractive: with no vars supplied yet, the template's
	// {{ vars.* }} refs are all "missing", which an autonomous render
	// would (wrongly) treat as a hard error — the missing-required-INPUT
	// check is applied separately below so the autonomous contract still
	// holds for the input layer.
	inputRender, err := spec.Render(bag.RenderRequest(agentlaunch.FrontEndInteractive))
	if err != nil {
		return agentlaunch.LaunchPlan{}, fmt.Errorf("specresolve: launch %q render: %w", launchID, err)
	}
	if frontEnd == agentlaunch.FrontEndAutonomous {
		if missing := inputMissing(inputRender.Missing); len(missing) > 0 {
			return agentlaunch.LaunchPlan{}, fmt.Errorf("%w: launch %q: %v",
				ErrMissingRequiredInputs, launchID, missing)
		}
	}

	// 3b. S4.2 var resolution over the input-expanded var sources.
	vars, err := r.resolveVars(ctx, spec, inputRender.ResolvedInputs)
	if err != nil {
		return agentlaunch.LaunchPlan{}, fmt.Errorf("specresolve: launch %q var resolution: %w", launchID, err)
	}

	// 4. Re-render with the resolved vars folded into the request.
	renderReq := bag.RenderRequest(frontEnd)
	renderReq.Vars = vars
	render, err := spec.Render(renderReq)
	if err != nil {
		return agentlaunch.LaunchPlan{}, fmt.Errorf("specresolve: launch %q render: %w", launchID, err)
	}

	// 4b. Bridge the minimum-config gap. The S4.4 minimum-config contract
	// is work_dir + runner only — `project` is a defaulted convenience
	// input and a minimum bag leaves it "". But agentlaunch's LaunchPlan
	// requires a non-empty Project.ID, and PlanFromLaunch derives the
	// project solely from the resolved `project` input. So a true
	// minimum-config bag would assemble an invalid plan. When `project`
	// is empty the resolver derives a stable fallback id from the
	// work_dir basename — the same slug the corpus uses for its non-
	// minimum bags (e.g. work_dir .../apps/tether -> project "tether").
	ensureProjectInput(render.ResolvedInputs)

	// 5. Resolve the runner -> RuntimeBinding and agent -> AgentSpec.
	// Either being unresolvable is a HARD error.
	runtime, err := r.resolveRunner(render.ResolvedInputs)
	if err != nil {
		return agentlaunch.LaunchPlan{}, fmt.Errorf("specresolve: launch %q: %w", launchID, err)
	}
	agent, err := r.resolveAgent(render.ResolvedInputs)
	if err != nil {
		return agentlaunch.LaunchPlan{}, fmt.Errorf("specresolve: launch %q: %w", launchID, err)
	}

	// 6. Assemble the plan. PlanFromLaunch validates the result.
	plan, err := agentlaunch.PlanFromLaunch(agentlaunch.PlanFromLaunchInput{
		Spec:    spec,
		Bag:     bag,
		Render:  render,
		Runtime: runtime,
		Agent:   agent,
		Mode:    modeForFrontEnd(frontEnd),
	})
	if err != nil {
		return agentlaunch.LaunchPlan{}, fmt.Errorf("specresolve: launch %q: %w", launchID, err)
	}
	return plan, nil
}

// loadBag loads and validates the LaunchBag for launchID. The bag file is
// <specsRoot>/launches/<launchID>.yaml — the filename stem is the id.
func (r *Resolver) loadBag(launchID string) (agentlaunch.LaunchBag, error) {
	path := filepath.Join(r.specsRoot, LaunchesDir, launchID+".yaml")
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return agentlaunch.LaunchBag{}, fmt.Errorf("%w: %q (no bag at %s)", ErrLaunchNotFound, launchID, path)
		}
		return agentlaunch.LaunchBag{}, fmt.Errorf("specresolve: stat launch bag %s: %w", path, err)
	}
	bag, err := agentlaunch.LoadLaunchBag(path)
	if err != nil {
		return agentlaunch.LaunchBag{}, fmt.Errorf("specresolve: load launch %q: %w", launchID, err)
	}
	return bag, nil
}

// loadSpec loads and validates the LaunchSpec a bag's `spec` ref names.
// The S5 corpus is one-spec-many-bags: every ref resolves to the single
// launch-assembly.yaml. A future per-spec layout would also be picked up
// here as <specsRoot>/<specRef>.yaml.
func (r *Resolver) loadSpec(specRef string) (agentlaunch.LaunchSpec, error) {
	if specRef == "" {
		return agentlaunch.LaunchSpec{}, fmt.Errorf("%w: bag names no spec", ErrSpecNotFound)
	}
	candidates := []string{
		filepath.Join(r.specsRoot, AssemblyFileName),
		filepath.Join(r.specsRoot, specRef+".yaml"),
	}
	var path string
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			path = c
			break
		}
	}
	if path == "" {
		return agentlaunch.LaunchSpec{}, fmt.Errorf("%w: %q (looked under %s)", ErrSpecNotFound, specRef, r.specsRoot)
	}
	spec, err := agentlaunch.LoadLaunchSpec(path)
	if err != nil {
		return agentlaunch.LaunchSpec{}, fmt.Errorf("specresolve: load spec %q: %w", specRef, err)
	}
	// Defend against an assembly file whose id does not match the bag ref
	// when a per-spec file was expected — only meaningful for the
	// per-spec candidate; the single-assembly corpus always matches.
	if filepath.Base(path) != AssemblyFileName && spec.ID != specRef {
		return agentlaunch.LaunchSpec{}, fmt.Errorf("%w: %q resolved to spec id %q", ErrSpecNotFound, specRef, spec.ID)
	}
	return spec, nil
}

// resolveVars runs S4.2 var resolution over spec's vars. The var sources
// are first expanded against the resolved inputs (the corpus parameterizes
// them with {{ inputs.* }} tags), then a VarResolver wired with the
// catalog TrustAuthorizer (D6c) and the http CallResolver resolves them.
//
// D1: every corpus var is authored on_error: warn, so a degraded source
// (recall endpoint down, role file missing, git unavailable) yields a
// warning and an empty/fallback value rather than a hard failure. A
// permanent failure (e.g. an unauthorized trust token) still aborts.
func (r *Resolver) resolveVars(ctx context.Context, spec agentlaunch.LaunchSpec, inputs map[string]any) (map[string]any, error) {
	if len(spec.Vars) == 0 {
		return map[string]any{}, nil
	}

	expanded := expandVarSources(spec.Vars, inputs)
	bootSpec := spec.BootSpec
	bootSpec.Vars = expanded

	vr := agentlaunch.NewVarResolver(agentlaunch.VarResolverOptions{
		Authorizer:   r.authorizer,
		CallResolver: r.callResolver,
	})

	// All corpus vars feed prompt text (session-start sink) and none are
	// required, so empty sink/required maps yield the documented defaults
	// (VarSinkPromptText, not-required).
	resolved, err := vr.ResolveAll(ctx, &bootSpec, nil, nil)
	if err != nil {
		// A permanent failure (e.g. an unauthorized trust token) aborts.
		// The partial map is discarded — the launch is not runnable with a
		// permanently-failed var.
		return nil, err
	}

	vars := make(map[string]any, len(resolved))
	for name, rv := range resolved {
		vars[name] = rv.Value
	}
	return vars, nil
}

// resolveRunner resolves the bag's `runner` input to a RuntimeBinding. An
// empty or unresolvable runner is a HARD error — there is no spec-baked
// runtime fallback (PlanFromLaunch §4.1).
func (r *Resolver) resolveRunner(inputs map[string]any) (agentlaunch.RuntimeBinding, error) {
	runner := stringInput(inputs, agentlaunch.LaunchInputRunner)
	if runner == "" {
		return agentlaunch.RuntimeBinding{}, fmt.Errorf("%w: bag supplies no runner", launchresolve.ErrRuntimeBindingNotFound)
	}
	binding, err := r.reg.ResolveRuntimeBinding(runner)
	if err != nil {
		return agentlaunch.RuntimeBinding{}, err
	}
	return binding, nil
}

// resolveAgent resolves the bag's `agent` input to an AgentSpec. Per the
// locked bridge-review decision the agent is a defaulted convenience input
// (it defaults to "general" on the spec), but once it has a value an
// unresolvable agent is a HARD error — a launch must not boot an agent
// identity the catalog does not know.
func (r *Resolver) resolveAgent(inputs map[string]any) (agentlaunch.AgentSpec, error) {
	agentID := stringInput(inputs, "agent")
	if agentID == "" {
		return agentlaunch.AgentSpec{}, fmt.Errorf("%w: bag supplies no agent", launchresolve.ErrAgentNotFound)
	}
	spec, err := r.reg.ResolveAgent(agentID)
	if err != nil {
		return agentlaunch.AgentSpec{}, err
	}
	return spec, nil
}

// ensureProjectInput guarantees the resolved-input map carries a non-empty
// `project` value, deriving one from the work_dir basename when the bag
// (e.g. a minimum-config bag) left it empty. This bridges the S4.4
// minimum-config contract (project optional) against the agentlaunch
// LaunchPlan contract (Project.ID required). It mutates the map in place;
// the map is the resolver's own freshly-rendered copy.
func ensureProjectInput(inputs map[string]any) {
	if inputs == nil {
		return
	}
	if p := stringInput(inputs, "project"); p != "" {
		return
	}
	workDir := stringInput(inputs, agentlaunch.LaunchInputWorkDir)
	if workDir == "" {
		return
	}
	inputs["project"] = filepath.Base(filepath.Clean(workDir))
}

// inputMissing filters a RenderResult.Missing list to the inputs.* entries
// — the unsatisfied required inputs. vars.* entries are not a render
// failure here: vars resolve via the VarResolver, not the bag.
func inputMissing(missing []string) []string {
	var out []string
	for _, m := range missing {
		if len(m) >= len("inputs.") && m[:len("inputs.")] == "inputs." {
			out = append(out, m)
		}
	}
	return out
}

// modeForFrontEnd maps a render front-end onto the LaunchMode stamped on
// the assembled plan: the interactive front-end is an interactive launch;
// the autonomous front-end is a background launch.
func modeForFrontEnd(fe agentlaunch.RenderFrontEnd) agentlaunch.LaunchMode {
	if fe == agentlaunch.FrontEndInteractive {
		return agentlaunch.LaunchInteractive
	}
	return agentlaunch.LaunchBackground
}

// stringInput reads key from a resolved-input map and coerces it to a
// string. A missing key or non-string value yields "".
func stringInput(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}
