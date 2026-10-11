// Package jev adapts the provider-neutral Decide operation to TypeSafe's JEV
// System One API (POST /v1/systemone). It is the only place JEV's wire shapes
// exist; nothing outside this package sees them.
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hollis-labs/tether/internal/llm"
)

const (
	// DefaultBaseURL is the JEV API host.
	DefaultBaseURL = "https://api.typesafe.ai"
	// DefaultTimeout bounds one attempt. JEV documents no timeout; calls are
	// typically ~100 ms, so this is generous without hanging a caller.
	DefaultTimeout = 15 * time.Second
	// DefaultMaxRetries is the number of retries after the first attempt for
	// 429 and 529 responses.
	DefaultMaxRetries = 3
	// DefaultBackoff is the first retry delay; it doubles each retry.
	DefaultBackoff = 250 * time.Millisecond

	// DefaultBreakerThreshold consecutive failed calls open the breaker.
	DefaultBreakerThreshold = 5
	// DefaultBreakerCooldown is how long an open breaker rejects calls.
	DefaultBreakerCooldown = 30 * time.Second

	maxResponseBytes = 4 << 20
	maxErrorBody     = 300
)

// Config configures one JEV provider instance.
type Config struct {
	ResolveAPIKey func(context.Context) (string, error)
	BaseURL       string
	HTTPClient    *http.Client
	// Timeout bounds each attempt. Zero uses DefaultTimeout.
	Timeout time.Duration
	// InputPriceUSDPerMTok is the flat price per million input tokens
	// (output is free). JEV is not in the models.dev catalog, so the provider
	// config declares it. Zero records no cost.
	InputPriceUSDPerMTok float64
	// MaxRetries after the first attempt, for 429/529 only. Negative disables
	// retries; zero uses DefaultMaxRetries.
	MaxRetries int
	// Backoff is the first retry delay. Zero uses DefaultBackoff.
	Backoff time.Duration
	// BreakerThreshold and BreakerCooldown tune the circuit breaker. Zero
	// uses the defaults.
	BreakerThreshold int
	BreakerCooldown  time.Duration

	// Sleep and Now are test seams.
	Sleep func(context.Context, time.Duration) error
	Now   func() time.Time
}

// Provider executes decision requests against JEV.
type Provider struct {
	cfg     Config
	client  *http.Client
	breaker *breaker
}

// New returns a Provider.
func New(cfg Config) *Provider {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = DefaultMaxRetries
	}
	if cfg.MaxRetries < 0 {
		cfg.MaxRetries = 0
	}
	if cfg.Backoff <= 0 {
		cfg.Backoff = DefaultBackoff
	}
	if cfg.BreakerThreshold <= 0 {
		cfg.BreakerThreshold = DefaultBreakerThreshold
	}
	if cfg.BreakerCooldown <= 0 {
		cfg.BreakerCooldown = DefaultBreakerCooldown
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleepCtx
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	return &Provider{cfg: cfg, client: client, breaker: &breaker{threshold: cfg.BreakerThreshold, cooldown: cfg.BreakerCooldown, now: cfg.Now}}
}

// Decide sends one decision request to JEV.
func (p *Provider) Decide(ctx context.Context, req llm.Request, route llm.RouteDecision) (llm.Response, error) {
	dec := req.Decision
	if err := dec.Validate(); err != nil {
		return llm.Response{}, err
	}
	fail := func(kind llm.DecisionErrorKind, status int, msg string) error {
		return &llm.DecisionError{Kind: kind, Status: status, OnError: dec.OnError, Message: msg}
	}

	if p.cfg.ResolveAPIKey == nil {
		return llm.Response{}, fail(llm.DecisionErrAuth, 0, "api key resolver is not configured")
	}
	key, err := p.cfg.ResolveAPIKey(ctx)
	if err != nil || key == "" {
		// The resolver error is dropped: it may echo the secret reference.
		return llm.Response{}, fail(llm.DecisionErrAuth, 0, "could not resolve api key")
	}

	body, err := buildBody(route.Model, dec)
	if err != nil {
		return llm.Response{}, fmt.Errorf("%w: %w", llm.ErrInvalidDecision, err)
	}

	if !p.breaker.allow() {
		return llm.Response{}, fail(llm.DecisionErrCircuitOpen, 0, "provider is failing; calls paused")
	}

	start := p.cfg.Now()
	wire, derr := p.call(ctx, key, body, dec.OnError)
	if derr != nil {
		p.breaker.record(derr.Kind)
		return llm.Response{}, derr
	}
	p.breaker.record("")

	answers, err := translateAnswers(dec, wire)
	if err != nil {
		return llm.Response{}, fail(llm.DecisionErrUnavailable, 0, err.Error())
	}
	model := wire.Model
	if model == "" {
		model = route.Model
	}
	latency := p.cfg.Now().Sub(start)
	usage := llm.Usage{
		InputTokens:  wire.Usage.InputTokens,
		OutputTokens: wire.Usage.OutputTokens,
	}
	if p.cfg.InputPriceUSDPerMTok > 0 {
		usage.EstimatedCostUSD = float64(usage.InputTokens) * p.cfg.InputPriceUSDPerMTok / 1e6
		usage.CostKind = "estimated"
	}
	return llm.Response{
		Provider: route.Provider,
		Model:    model,
		Usage:    usage,
		Decision: &llm.DecisionResult{
			Answers:   answers,
			LatencyMS: latency.Milliseconds(),
			RequestID: req.RequestID,
		},
	}, nil
}

// call performs the HTTP exchange with bounded retries for 429/529.
func (p *Provider) call(ctx context.Context, key string, body []byte, onErr llm.OnError) (*wireResponse, *llm.DecisionError) {
	fail := func(kind llm.DecisionErrorKind, status int, msg string) *llm.DecisionError {
		return &llm.DecisionError{Kind: kind, Status: status, OnError: onErr, Message: msg}
	}
	delay := p.cfg.Backoff
	for attempt := 0; ; attempt++ {
		status, raw, err := p.attempt(ctx, key, body)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, fail(llm.DecisionErrTimeout, 0, "request timed out")
			}
			if ctx.Err() != nil {
				return nil, fail(llm.DecisionErrUnavailable, 0, "request canceled")
			}
			return nil, fail(llm.DecisionErrUnavailable, 0, "transport error")
		}
		switch {
		case status == http.StatusOK:
			var out wireResponse
			if err := json.Unmarshal(raw, &out); err != nil {
				return nil, fail(llm.DecisionErrUnavailable, status, "undecodable response")
			}
			return &out, nil
		case status == http.StatusUnauthorized:
			return nil, fail(llm.DecisionErrAuth, status, "credentials rejected")
		case status == http.StatusUnprocessableEntity:
			return nil, fail(llm.DecisionErrInvalid, status, errorDetail(raw))
		case status == http.StatusTooManyRequests || status == 529:
			if attempt >= p.cfg.MaxRetries {
				kind := llm.DecisionErrRateLimited
				if status == 529 {
					kind = llm.DecisionErrOverloaded
				}
				return nil, fail(kind, status, fmt.Sprintf("gave up after %d attempts", attempt+1))
			}
			if err := p.cfg.Sleep(ctx, delay); err != nil {
				return nil, fail(llm.DecisionErrUnavailable, status, "request canceled while backing off")
			}
			delay *= 2
		default:
			return nil, fail(llm.DecisionErrUnavailable, status, "unexpected response")
		}
	}
}

func (p *Provider) attempt(ctx context.Context, key string, body []byte) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.BaseURL+"/v1/systemone", bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+key)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, raw, nil
}

// errorDetail extracts a short, safe description from a 422 body. The body
// names the offending field; it is truncated and never includes request state.
func errorDetail(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > maxErrorBody {
		s = s[:maxErrorBody] + "..."
	}
	if s == "" {
		return "provider rejected the request"
	}
	return "provider rejected the request: " + s
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// breaker opens after threshold consecutive failed calls and rejects calls
// until the cooldown passes, then lets one probe through. Invalid and auth
// failures describe the request, not the provider's health, and do not count.
type breaker struct {
	mu        sync.Mutex
	threshold int
	cooldown  time.Duration
	now       func() time.Time
	failures  int
	openUntil time.Time
	probing   bool
}

func (b *breaker) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openUntil.IsZero() {
		return true
	}
	if b.now().Before(b.openUntil) || b.probing {
		return false
	}
	b.probing = true
	return true
}

func (b *breaker) record(kind llm.DecisionErrorKind) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.probing = false
	switch kind {
	case "", llm.DecisionErrInvalid, llm.DecisionErrAuth:
		b.failures = 0
		b.openUntil = time.Time{}
	default:
		b.failures++
		if b.failures >= b.threshold {
			b.openUntil = b.now().Add(b.cooldown)
		}
	}
}
