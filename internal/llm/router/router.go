package router

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/hollis-labs/go-modelsdev/modelsdev"
	"github.com/hollis-labs/tether/internal/llm"
)

var (
	ErrNoCandidates = errors.New("no route candidates available")
	ErrNoRouteMatch = errors.New("no route satisfied the request")
)

// Catalog describes the model metadata the planner needs.
type Catalog interface {
	List() []modelsdev.ModelRef
	Get(providerID, modelID string) (modelsdev.Model, bool)
	Capabilities(providerID, modelID string) (modelsdev.Capabilities, bool)
	Modality(providerID, modelID string) (modelsdev.Modality, bool)
	ContextWindow(providerID, modelID string) (int, bool)
	MaxOutput(providerID, modelID string) (int, bool)
	EstimateCost(providerID, modelID string, promptTokens, completionTokens int) (float64, bool)
}

// Route is one explicit provider/model target the planner may consider.
type Route struct {
	Provider          string
	CatalogProvider   string
	Model             string
	Mode              string
	Intent            string
	RequiresReasoning bool
	RequiresTools     bool
	AllowReasoning    *bool
	AllowTools        *bool
	AllowAttachments  *bool
	MaxOutputTokens   *int
	MaxCostUSD        *float64
	UsageBudget       UsageBudgetPolicy
}

type UsageBudgetPolicy struct {
	Level      string
	MaxCostUSD *float64
	Window     string
	Scope      string
}

// Policy controls planner ordering and audit versioning.
type Policy struct {
	Version string
	Routes  []Route
}

// PolicyEvaluator can reject a candidate route using runtime state outside the
// static request/catalog planner inputs.
type PolicyEvaluator interface {
	Validate(req llm.Request, route Route, estimatedCostUSD float64) error
}

// Plan is the chosen provider/model plus compact audit metadata.
type Plan struct {
	Provider         string
	Model            string
	EstimatedCostUSD float64
	Reasons          []string
	PolicyVersion    string
}

// RouteDecision returns the normalized route decision payload for llm.Response.
func (p Plan) RouteDecision() llm.RouteDecision {
	return llm.RouteDecision{
		Provider:      p.Provider,
		Model:         p.Model,
		Reasons:       slices.Clone(p.Reasons),
		PolicyVersion: p.PolicyVersion,
	}
}

// Explanation is a structured planner trace for one request. It includes the
// selected winner when one exists plus per-route match/evaluation details.
type Explanation struct {
	PolicyVersion string
	Winner        *Plan
	Error         string
	Candidates    []CandidateExplanation
}

// CandidateExplanation records why one route was selected, rejected, or
// skipped for a given request.
type CandidateExplanation struct {
	Route            Route
	Matched          bool
	Selected         bool
	Reasons          []string
	Error            string
	EstimatedCostUSD float64
}

// Planner plans provider/model routes using configured order and catalog
// metadata.
type Planner struct {
	catalog    Catalog
	policy     Policy
	evaluators []PolicyEvaluator
}

// New returns a Planner.
func New(catalog Catalog, policy Policy) *Planner {
	return NewWithEvaluators(catalog, policy)
}

// NewWithEvaluators returns a Planner with optional runtime policy
// evaluators.
func NewWithEvaluators(catalog Catalog, policy Policy, evaluators ...PolicyEvaluator) *Planner {
	return &Planner{catalog: catalog, policy: policy, evaluators: append([]PolicyEvaluator(nil), evaluators...)}
}

// Plan selects one provider/model pair for req or returns an error when no
// configured candidate satisfies the request.
func (p *Planner) Plan(req llm.Request) (Plan, error) {
	candidates := p.candidates(req)
	if len(candidates) == 0 {
		return Plan{}, ErrNoCandidates
	}

	var failures []string
	hadFallback := false
	for _, candidate := range candidates {
		plan, err := p.evaluateCandidate(req, candidate)
		if err == nil {
			if hadFallback {
				plan.Reasons = append(plan.Reasons, "selected after ordered fallback")
			}
			return plan, nil
		}
		hadFallback = true
		failures = append(failures, fmt.Sprintf("%s/%s: %v", candidate.Provider, candidate.Model, err))
	}

	return Plan{}, fmt.Errorf("%w: %s", ErrNoRouteMatch, strings.Join(failures, "; "))
}

// Explain returns a structured planner trace for req, including failures.
func (p *Planner) Explain(req llm.Request) Explanation {
	explanation := Explanation{PolicyVersion: p.policy.Version}
	base := dedupeRoutes(p.baseRoutes())
	candidates := p.candidates(req)
	if len(candidates) == 0 {
		explanation.Error = ErrNoCandidates.Error()
		explanation.Candidates = make([]CandidateExplanation, 0, len(base))
		for _, route := range base {
			explanation.Candidates = append(explanation.Candidates, CandidateExplanation{
				Route:   route,
				Reasons: exclusionReasons(route, req),
			})
		}
		return explanation
	}

	selectedKey := ""
	candidateDetails := map[string]CandidateExplanation{}
	failuresBeforeWinner := 0
	var winner *Plan
	for _, candidate := range candidates {
		detail := CandidateExplanation{
			Route:   candidate,
			Matched: true,
		}
		plan, err := p.evaluateCandidate(req, candidate)
		if err != nil {
			detail.Error = err.Error()
			detail.Reasons = append(detail.Reasons, selectionReasons(req, candidate)...)
			failuresBeforeWinner++
		} else {
			detail.EstimatedCostUSD = plan.EstimatedCostUSD
			detail.Reasons = append(detail.Reasons, plan.Reasons...)
			if winner == nil {
				if failuresBeforeWinner > 0 {
					detail.Reasons = append(detail.Reasons, "selected after ordered fallback")
				}
				detail.Selected = true
				selectedKey = routeKey(candidate)
				winnerCopy := plan
				winner = &winnerCopy
			}
		}
		candidateDetails[routeKey(candidate)] = detail
	}

	if winner != nil {
		explanation.Winner = winner
	} else {
		var failures []string
		for _, candidate := range candidates {
			if detail, ok := candidateDetails[routeKey(candidate)]; ok && detail.Error != "" {
				failures = append(failures, fmt.Sprintf("%s/%s: %s", candidate.Provider, candidate.Model, detail.Error))
			}
		}
		explanation.Error = fmt.Errorf("%w: %s", ErrNoRouteMatch, strings.Join(failures, "; ")).Error()
	}

	explanation.Candidates = make([]CandidateExplanation, 0, len(base))
	for _, route := range base {
		key := routeKey(route)
		if detail, ok := candidateDetails[key]; ok {
			if key == selectedKey {
				detail.Selected = true
			}
			explanation.Candidates = append(explanation.Candidates, detail)
			continue
		}
		explanation.Candidates = append(explanation.Candidates, CandidateExplanation{
			Route:   route,
			Reasons: exclusionReasons(route, req),
		})
	}
	return explanation
}

func (p *Planner) baseRoutes() []Route {
	if len(p.policy.Routes) > 0 {
		return append([]Route(nil), p.policy.Routes...)
	}
	return routesFromCatalog(p.catalog)
}

func (p *Planner) candidates(req llm.Request) []Route {
	base := p.policy.Routes
	if len(base) == 0 {
		base = routesFromCatalog(p.catalog)
	}

	filtered := make([]Route, 0, len(base))
	for _, route := range base {
		if !routeMatches(route, req) {
			continue
		}
		if req.ProviderHint != "" && route.Provider != req.ProviderHint {
			continue
		}
		if req.ModelHint != "" && route.Model != req.ModelHint {
			continue
		}
		filtered = append(filtered, route)
	}
	if len(filtered) > 0 {
		if len(p.policy.Routes) > 0 {
			sort.SliceStable(filtered, func(i, j int) bool {
				return routeSpecificity(filtered[i]) > routeSpecificity(filtered[j])
			})
		}
		return dedupeRoutes(filtered)
	}
	if len(p.policy.Routes) > 0 {
		switch {
		case req.ProviderHint != "" && req.ModelHint != "":
			return []Route{{Provider: req.ProviderHint, Model: req.ModelHint}}
		case req.ProviderHint != "":
			return dedupeRoutes(append([]Route{{Provider: req.ProviderHint}}, routesForProvider(p.catalog, req.ProviderHint)...))
		case req.ModelHint != "":
			return dedupeRoutes(append([]Route{{Model: req.ModelHint}}, routesForModel(p.catalog, req.ModelHint)...))
		default:
			return nil
		}
	}

	switch {
	case req.ProviderHint != "" && req.ModelHint != "":
		return []Route{{Provider: req.ProviderHint, Model: req.ModelHint}}
	case req.ProviderHint != "":
		return dedupeRoutes(append([]Route{{Provider: req.ProviderHint}}, routesForProvider(p.catalog, req.ProviderHint)...))
	case req.ModelHint != "":
		return dedupeRoutes(append([]Route{{Model: req.ModelHint}}, routesForModel(p.catalog, req.ModelHint)...))
	default:
		return dedupeRoutes(base)
	}
}

func (p *Planner) evaluateCandidate(req llm.Request, candidate Route) (Plan, error) {
	provider := candidate.Provider
	catalogProvider := candidate.CatalogProvider
	if catalogProvider == "" {
		catalogProvider = provider
	}
	model := candidate.Model
	if provider == "" || model == "" {
		return Plan{}, fmt.Errorf("route missing provider or model")
	}

	if err := validateRoutePolicy(req, candidate); err != nil {
		return Plan{}, err
	}

	if _, ok := p.catalog.Get(catalogProvider, model); !ok {
		return Plan{}, fmt.Errorf("model not found in catalog")
	}

	if err := validateCapabilities(req, p.catalog, catalogProvider, model); err != nil {
		return Plan{}, err
	}
	if err := validateLimits(req, p.catalog, catalogProvider, model); err != nil {
		return Plan{}, err
	}

	plan := Plan{
		Provider:      provider,
		Model:         model,
		PolicyVersion: p.policy.Version,
	}
	plan.Reasons = append(plan.Reasons, selectionReasons(req, candidate)...)

	if cost, ok, err := validateBudget(req, p.catalog, catalogProvider, model); err != nil {
		return Plan{}, err
	} else if ok {
		plan.EstimatedCostUSD = cost
		plan.Reasons = append(plan.Reasons, fmt.Sprintf("estimated cost %.6f USD within budget", cost))
	}
	for _, evaluator := range p.evaluators {
		if evaluator == nil {
			continue
		}
		if err := evaluator.Validate(req, candidate, plan.EstimatedCostUSD); err != nil {
			return Plan{}, err
		}
	}

	return plan, nil
}

func routesFromCatalog(c Catalog) []Route {
	if c == nil {
		return nil
	}
	refs := c.List()
	routes := make([]Route, 0, len(refs))
	for _, ref := range refs {
		routes = append(routes, Route{Provider: ref.ProviderID, Model: ref.ID})
	}
	return routes
}

func routesForProvider(c Catalog, provider string) []Route {
	if c == nil {
		return nil
	}
	refs := c.List()
	routes := make([]Route, 0, len(refs))
	for _, ref := range refs {
		if ref.ProviderID == provider {
			routes = append(routes, Route{Provider: ref.ProviderID, Model: ref.ID})
		}
	}
	return routes
}

func routesForModel(c Catalog, model string) []Route {
	if c == nil {
		return nil
	}
	refs := c.List()
	routes := make([]Route, 0, len(refs))
	for _, ref := range refs {
		if ref.ID == model {
			routes = append(routes, Route{Provider: ref.ProviderID, Model: ref.ID})
		}
	}
	return routes
}

func dedupeRoutes(in []Route) []Route {
	out := make([]Route, 0, len(in))
	seen := map[string]struct{}{}
	for _, route := range in {
		key := routeKey(route)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, route)
	}
	return out
}

func routeKey(route Route) string {
	key := route.Provider + "\x00" + route.Model
	key += "\x00" + strings.ToLower(strings.TrimSpace(route.Mode))
	key += "\x00" + strings.ToLower(strings.TrimSpace(route.Intent))
	if route.RequiresReasoning {
		key += "\x00reasoning"
	}
	if route.RequiresTools {
		key += "\x00tools"
	}
	if route.AllowReasoning != nil {
		key += fmt.Sprintf("\x00allow_reasoning=%t", *route.AllowReasoning)
	}
	if route.AllowTools != nil {
		key += fmt.Sprintf("\x00allow_tools=%t", *route.AllowTools)
	}
	if route.AllowAttachments != nil {
		key += fmt.Sprintf("\x00allow_attachments=%t", *route.AllowAttachments)
	}
	if route.MaxOutputTokens != nil {
		key += fmt.Sprintf("\x00max_output=%d", *route.MaxOutputTokens)
	}
	if route.MaxCostUSD != nil {
		key += fmt.Sprintf("\x00max_cost=%.6f", *route.MaxCostUSD)
	}
	if route.UsageBudget.Level != "" {
		key += "\x00usage_budget_level=" + route.UsageBudget.Level
	}
	if route.UsageBudget.MaxCostUSD != nil {
		key += fmt.Sprintf("\x00usage_budget_max_cost=%.6f", *route.UsageBudget.MaxCostUSD)
	}
	if route.UsageBudget.Window != "" {
		key += "\x00usage_budget_window=" + route.UsageBudget.Window
	}
	if route.UsageBudget.Scope != "" {
		key += "\x00usage_budget_scope=" + route.UsageBudget.Scope
	}
	return key
}

func validateRoutePolicy(req llm.Request, route Route) error {
	if route.AllowReasoning != nil && !*route.AllowReasoning && requiresReasoning(req) {
		return fmt.Errorf("route policy disallows reasoning")
	}
	if route.AllowTools != nil && !*route.AllowTools && len(req.Tools) > 0 {
		return fmt.Errorf("route policy disallows tools")
	}
	if route.AllowAttachments != nil && !*route.AllowAttachments && requiresAttachmentSupport(req) {
		return fmt.Errorf("route policy disallows attachments")
	}
	if route.MaxOutputTokens != nil && req.MaxOutputTokens > 0 && req.MaxOutputTokens > *route.MaxOutputTokens {
		return fmt.Errorf("route policy max output %d exceeded by request %d", *route.MaxOutputTokens, req.MaxOutputTokens)
	}
	if route.MaxCostUSD != nil && req.CostBudgetUSD > 0 && req.CostBudgetUSD > *route.MaxCostUSD {
		return fmt.Errorf("route policy max cost %.6f USD exceeded by request budget %.6f USD", *route.MaxCostUSD, req.CostBudgetUSD)
	}
	return nil
}

func validateCapabilities(req llm.Request, c Catalog, provider, model string) error {
	caps, _ := c.Capabilities(provider, model)
	modality, _ := c.Modality(provider, model)

	if len(req.Tools) > 0 && !caps.ToolCall {
		return fmt.Errorf("tool calling unsupported")
	}
	if requiresReasoning(req) && !caps.Reasoning {
		return fmt.Errorf("reasoning unsupported")
	}
	if requiresAttachmentSupport(req) && !caps.Attachment {
		return fmt.Errorf("attachments unsupported")
	}

	requiredInputs := requiredInputModalities(req)
	for _, input := range requiredInputs {
		if !contains(modality.Input, input) {
			return fmt.Errorf("input modality %q unsupported", input)
		}
	}
	return nil
}

func validateLimits(req llm.Request, c Catalog, provider, model string) error {
	if req.MaxOutputTokens > 0 {
		if maxOut, ok := c.MaxOutput(provider, model); ok && req.MaxOutputTokens > maxOut {
			return fmt.Errorf("requested max output %d exceeds model max output %d", req.MaxOutputTokens, maxOut)
		}
	}

	tokenNeed := req.MaxInputTokens
	if req.TokenBudget > tokenNeed {
		tokenNeed = req.TokenBudget
	}
	if req.MaxInputTokens > 0 && req.MaxOutputTokens > 0 && req.MaxInputTokens+req.MaxOutputTokens > tokenNeed {
		tokenNeed = req.MaxInputTokens + req.MaxOutputTokens
	}
	if tokenNeed > 0 {
		if ctxWindow, ok := c.ContextWindow(provider, model); ok && tokenNeed > ctxWindow {
			return fmt.Errorf("requested token budget %d exceeds context window %d", tokenNeed, ctxWindow)
		}
	}
	return nil
}

func validateBudget(req llm.Request, c Catalog, provider, model string) (float64, bool, error) {
	if req.CostBudgetUSD <= 0 {
		return 0, false, nil
	}
	promptTokens, completionTokens, ok := budgetEstimateTokens(req)
	if !ok {
		return 0, false, nil
	}
	cost, ok := c.EstimateCost(provider, model, promptTokens, completionTokens)
	if !ok {
		return 0, false, nil
	}
	if cost > req.CostBudgetUSD {
		return 0, true, fmt.Errorf("estimated cost %.6f USD exceeds budget %.6f USD", cost, req.CostBudgetUSD)
	}
	return cost, true, nil
}

func budgetEstimateTokens(req llm.Request) (promptTokens, completionTokens int, ok bool) {
	switch {
	case req.MaxInputTokens > 0 || req.MaxOutputTokens > 0:
		promptTokens = req.MaxInputTokens
		completionTokens = req.MaxOutputTokens
		if promptTokens == 0 && req.TokenBudget > completionTokens {
			promptTokens = req.TokenBudget - completionTokens
		}
		if completionTokens == 0 && req.TokenBudget > promptTokens {
			completionTokens = req.TokenBudget - promptTokens
		}
		return promptTokens, completionTokens, promptTokens+completionTokens > 0
	case req.TokenBudget > 0:
		return req.TokenBudget, 0, true
	default:
		return 0, 0, false
	}
}

func selectionReasons(req llm.Request, candidate Route) []string {
	var reasons []string
	if req.ProviderHint != "" && candidate.Provider == req.ProviderHint {
		reasons = append(reasons, "matched provider hint")
	}
	if req.ModelHint != "" && candidate.Model == req.ModelHint {
		reasons = append(reasons, "matched model hint")
	}
	if len(reasons) == 0 {
		reasons = append(reasons, "selected first configured route that satisfied policy")
	}
	if candidate.Mode != "" {
		reasons = append(reasons, fmt.Sprintf("matched route mode %q", candidate.Mode))
	}
	if candidate.Intent != "" {
		reasons = append(reasons, fmt.Sprintf("matched route intent %q", candidate.Intent))
	}
	if candidate.RequiresReasoning {
		reasons = append(reasons, "matched route reasoning requirement")
	}
	if candidate.RequiresTools {
		reasons = append(reasons, "matched route tools requirement")
	}
	if len(req.Tools) > 0 {
		reasons = append(reasons, "supports tool calling")
	}
	if requiresReasoning(req) {
		reasons = append(reasons, "supports reasoning")
	}
	if requiresAttachmentSupport(req) {
		reasons = append(reasons, "supports attachments")
	}
	for _, input := range requiredInputModalities(req) {
		if input == "text" {
			continue
		}
		reasons = append(reasons, fmt.Sprintf("supports %s input", input))
	}
	return reasons
}

func routeMatches(route Route, req llm.Request) bool {
	if route.Mode != "" && !strings.EqualFold(strings.TrimSpace(route.Mode), strings.TrimSpace(req.Mode)) {
		return false
	}
	if route.Intent != "" && !strings.EqualFold(strings.TrimSpace(route.Intent), strings.TrimSpace(req.Intent)) {
		return false
	}
	if route.RequiresReasoning && !requiresReasoning(req) {
		return false
	}
	if route.RequiresTools && len(req.Tools) == 0 {
		return false
	}
	return true
}

func routeSpecificity(route Route) int {
	score := 0
	if route.Mode != "" {
		score++
	}
	if route.Intent != "" {
		score++
	}
	if route.RequiresReasoning {
		score++
	}
	if route.RequiresTools {
		score++
	}
	return score
}

func exclusionReasons(route Route, req llm.Request) []string {
	var reasons []string
	if route.Mode != "" && !strings.EqualFold(strings.TrimSpace(route.Mode), strings.TrimSpace(req.Mode)) {
		reasons = append(reasons, fmt.Sprintf("route mode %q did not match request mode %q", route.Mode, req.Mode))
	}
	if route.Intent != "" && !strings.EqualFold(strings.TrimSpace(route.Intent), strings.TrimSpace(req.Intent)) {
		reasons = append(reasons, fmt.Sprintf("route intent %q did not match request intent %q", route.Intent, req.Intent))
	}
	if route.RequiresReasoning && !requiresReasoning(req) {
		reasons = append(reasons, "route requires reasoning request")
	}
	if route.RequiresTools && len(req.Tools) == 0 {
		reasons = append(reasons, "route requires tool-capable request")
	}
	if route.AllowReasoning != nil && !*route.AllowReasoning && requiresReasoning(req) {
		reasons = append(reasons, "route policy disallows reasoning")
	}
	if route.AllowTools != nil && !*route.AllowTools && len(req.Tools) > 0 {
		reasons = append(reasons, "route policy disallows tools")
	}
	if route.AllowAttachments != nil && !*route.AllowAttachments && requiresAttachmentSupport(req) {
		reasons = append(reasons, "route policy disallows attachments")
	}
	if route.MaxOutputTokens != nil && req.MaxOutputTokens > 0 && req.MaxOutputTokens > *route.MaxOutputTokens {
		reasons = append(reasons, fmt.Sprintf("route policy max output %d is below requested %d", *route.MaxOutputTokens, req.MaxOutputTokens))
	}
	if route.MaxCostUSD != nil && req.CostBudgetUSD > 0 && req.CostBudgetUSD > *route.MaxCostUSD {
		reasons = append(reasons, fmt.Sprintf("route policy max cost %.6f USD is below requested budget %.6f USD", *route.MaxCostUSD, req.CostBudgetUSD))
	}
	if req.ProviderHint != "" && route.Provider != req.ProviderHint {
		reasons = append(reasons, fmt.Sprintf("provider hint %q excluded route", req.ProviderHint))
	}
	if req.ModelHint != "" && route.Model != req.ModelHint {
		reasons = append(reasons, fmt.Sprintf("model hint %q excluded route", req.ModelHint))
	}
	if len(reasons) == 0 {
		reasons = append(reasons, "route was not evaluated")
	}
	return reasons
}

func requiresReasoning(req llm.Request) bool {
	intent := strings.ToLower(strings.TrimSpace(req.Intent))
	return intent == "reasoning" || strings.Contains(intent, "reason")
}

func requiresAttachmentSupport(req llm.Request) bool {
	if len(req.Attachments) > 0 {
		return true
	}
	for _, msg := range req.Input {
		for _, part := range msg.Parts {
			switch strings.ToLower(strings.TrimSpace(part.Type)) {
			case "image", "audio", "file":
				return true
			}
		}
	}
	return false
}

func requiredInputModalities(req llm.Request) []string {
	set := map[string]struct{}{"text": {}}
	for _, msg := range req.Input {
		for _, part := range msg.Parts {
			switch strings.ToLower(strings.TrimSpace(part.Type)) {
			case "image", "audio":
				set[part.Type] = struct{}{}
			}
		}
	}
	for _, attachment := range req.Attachments {
		switch strings.ToLower(strings.TrimSpace(attachment.Type)) {
		case "image", "audio":
			set[attachment.Type] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for _, modality := range []string{"text", "image", "audio"} {
		if _, ok := set[modality]; ok {
			out = append(out, modality)
		}
	}
	return out
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
