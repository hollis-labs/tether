package events

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sync"

	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents"
	"github.com/hollis-labs/libs/harness/core/eventstage"
)

// StageMode is selected once at startup. A library bump never enables enforcement.
type StageMode string

const (
	Shadow  StageMode = "shadow"
	Enforce StageMode = "enforce"
)

// StageCounts contains only aggregate metadata. Unknown kinds share one bucket
// so neither attacker-controlled kind strings nor unbounded cardinality survive.
type StageCounts struct {
	Events, Changed, Redactions, Failures uint64
	Class                                 AdmissionClass
	Errors                                map[string]uint64
}

// Pipeline adapts Tether's local envelope to the released shared stage chain.
// Only payload/class are projected into runtimeevents; actor and dimensions
// remain exclusively owned by the existing verified stamp stages.
type Pipeline struct {
	mode     StageMode
	policies map[string]admissionPolicy
	chain    *eventstage.Chain[pipelineEvent]
	mu       sync.Mutex
	counts   map[string]StageCounts
}
type pipelineEvent struct {
	candidate Event
	stamped   Event
	stamp     func(context.Context, Event) Event
	runtime   runtimeevents.Event
}

// NewPipeline constructs the shared chain using released payload policies.
func NewPipeline(mode StageMode) (*Pipeline, error) { return newPipeline(mode, nil) }

func newPipeline(mode StageMode, sanitizer eventstage.PayloadSanitizer) (*Pipeline, error) {
	if mode == "" {
		mode = Shadow
	}
	if mode != Shadow && mode != Enforce {
		return nil, fmt.Errorf("event_stage_mode must be shadow or enforce")
	}
	p := &Pipeline{mode: mode, policies: pipelinePolicies(), counts: map[string]StageCounts{}}
	rules, err := eventstage.ParseRules([]byte(`{"schema":"eventstage.rules/v1","id":"tether-events","version":"2026-10-19.1","rules":[],"tests":[{"name":"usage catalog","event":{"kind":"session.usage"},"want":{"category":"activity"}},{"name":"health catalog","event":{"kind":"session.health_changed"},"want":{"category":"state"}}]}`))
	if err != nil {
		return nil, err
	}
	if failures := rules.SelfTest(context.Background()); len(failures) > 0 {
		return nil, failures[0]
	}
	if sanitizer == nil {
		sanitizer = payloadPolicies(p.policies)
	}
	sanitize := pipelineSanitize{known: p.policies, inner: eventstage.RuntimeEventsSanitize(sanitizer, eventstage.ModeProducer)}
	classify := eventstage.RuntimeEventsClassify(rules)
	p.chain, err = eventstage.NewChain(eventstage.Config[pipelineEvent]{
		Stamp: eventstage.Named("stamp/tether", func(ctx context.Context, e *pipelineEvent) (eventstage.Outcome, error) {
			e.candidate = e.stamp(ctx, e.candidate)
			e.stamped = cloneEvent(e.candidate)
			return eventstage.Outcome{Changed: true}, nil
		}),
		Sanitize: sanitize,
		Classify: eventstage.Named("classify/tether", func(ctx context.Context, e *pipelineEvent) (eventstage.Outcome, error) {
			// Compatibility diagnostics are not an admission policy; no B body may
			// enter classification, nor may a failed-closed/tombstoned payload.
			if p.policies[e.candidate.Kind].class != Metadata || e.runtime.Tombstoned() || e.runtime.Class == nil || !e.runtime.Class.Sanitized {
				return eventstage.Outcome{}, nil
			}
			out, err := classify.Apply(ctx, &e.runtime)
			copyRuntimeClass(&e.candidate, e.runtime)
			return out, err
		}),
	})
	return p, err
}

func (p *Pipeline) apply(ctx context.Context, e Event, stamp func(context.Context, Event) Event) Event {
	state := pipelineEvent{candidate: cloneEvent(e), stamped: cloneEvent(e), stamp: stamp}
	report := p.chain.Apply(ctx, &state)
	// The adapters never drop, and FailClosed is value-free. If a future adapter
	// cannot fail closed, preserve the event envelope with an error payload.
	if !report.Emit() {
		state.candidate.PayloadJSON = `{"sanitize_error":"stage_error"}`
		state.candidate.Envelope.Class.Sanitized = false
	}
	policy, known := p.policies[e.Kind]
	p.record(e.Kind, policy.class, known, state.stamped, state.candidate, report)
	if p.mode == Shadow {
		return state.stamped
	}
	// Retention tombstones are not new producer bodies. Preserve their whole
	// existing class, including an independently proven sanitized=true flag.
	if state.stamped.Envelope.PayloadDigest != "" {
		payload := bytes.TrimSpace([]byte(state.stamped.PayloadJSON))
		if len(payload) == 0 || bytes.Equal(payload, []byte("null")) {
			return state.stamped
		}
		// Invalid digest+body is an envelope invariant, not B text policy. The
		// shared FailClosed result must survive the compatibility return.
		return state.candidate
	}
	if policy.class == Compatibility {
		state.stamped.Envelope.Class.Sanitized = false
		return state.stamped
	}
	return state.candidate
}

// Counts returns detached, value-free source evidence; it is not live rollout
// acceptance and contains neither payloads, field paths, nor raw errors.
func (p *Pipeline) Counts() map[string]StageCounts {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]StageCounts, len(p.counts))
	for k, v := range p.counts {
		cp := make(map[string]uint64, len(v.Errors))
		for key, n := range v.Errors {
			cp[key] = n
		}
		v.Errors = cp
		out[k] = v
	}
	return out
}

func (p *Pipeline) record(kind string, class AdmissionClass, known bool, before, after Event, report eventstage.Report) {
	if !known {
		kind = "<unregistered>"
		class = Unregistered
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	count := p.counts[kind]
	count.Class = class
	count.Events++
	if before.PayloadJSON != after.PayloadJSON {
		count.Changed++
	}
	if n := after.Envelope.Class.Redactions - before.Envelope.Class.Redactions; n > 0 {
		count.Redactions += uint64(n)
	}
	if report.Err() != nil {
		count.Failures++
		if count.Errors == nil {
			count.Errors = map[string]uint64{}
		}
		var payload struct {
			Class string `json:"sanitize_error"`
		}
		_ = json.Unmarshal([]byte(after.PayloadJSON), &payload)
		switch payload.Class {
		case "unknown_kind", "oversized", "invalid_json", "opaque", "depth", "tombstone_with_payload", "redactions_overflow":
		default:
			payload.Class = "stage_error"
		}
		count.Errors[payload.Class]++
	}
	p.counts[kind] = count
}

type pipelineSanitize struct {
	known map[string]admissionPolicy
	inner eventstage.FailClosedStage[runtimeevents.Event]
}

func (pipelineSanitize) Name() string { return "sanitize/tether" }
func (s pipelineSanitize) Apply(ctx context.Context, e *pipelineEvent) (eventstage.Outcome, error) {
	c := e.candidate.Envelope.Class
	e.runtime = runtimeevents.Event{Kind: runtimeevents.EventKind(e.candidate.Kind), Payload: json.RawMessage(e.candidate.PayloadJSON), PayloadDigest: e.candidate.Envelope.PayloadDigest, Class: &runtimeevents.Class{Category: c.Category, Severity: c.Severity, Sensitivity: c.Sensitivity, PII: c.PII, Sanitized: c.Sanitized, Labels: slices.Clone(c.Labels)}}
	if _, known := s.known[e.candidate.Kind]; !known && !e.runtime.Tombstoned() {
		return eventstage.Outcome{}, pipelineError("unknown_kind")
	}
	if c.Redactions < 0 || uint64(c.Redactions) > min(uint64(math.MaxUint32), uint64(^uint(0)>>1)) {
		return eventstage.Outcome{}, pipelineError("redactions_overflow")
	}
	// Preserve only exact known fail-closed observations. Normal producer
	// input, including anything claiming sanitized=true, still runs Producer
	// sanitation. Canonical compact matching rejects extra/duplicate fields.
	if !c.Sanitized && !e.runtime.Tombstoned() && priorFailedClosed(e.candidate.PayloadJSON) {
		e.runtime.Class.Redactions = uint32(c.Redactions)
		return eventstage.Outcome{Note: eventstage.NoteBackstopFailedClosed}, nil
	}
	// Reset the adapter's uint32 counter, then add in uint64 after sanitation.
	// This prevents the shared adapter's uint32 addition from wrapping.
	out, err := s.inner.Apply(ctx, &e.runtime)
	if err != nil {
		return out, err
	}
	if uint64(c.Redactions)+uint64(e.runtime.Class.Redactions) > min(uint64(math.MaxUint32), uint64(^uint(0)>>1)) {
		return eventstage.Outcome{}, pipelineError("redactions_overflow")
	}
	e.runtime.Class.Redactions += uint32(c.Redactions)
	copyRuntimeClass(&e.candidate, e.runtime)
	return out, nil
}
func (s pipelineSanitize) FailClosed(e *pipelineEvent, cause error) {
	s.inner.FailClosed(&e.runtime, cause)
	e.candidate.Envelope.Truncated = false
	copyRuntimeClass(&e.candidate, e.runtime)
}
func copyRuntimeClass(e *Event, r runtimeevents.Event) {
	e.PayloadJSON = string(r.Payload)
	e.Envelope.PayloadDigest = r.PayloadDigest
	if r.Class != nil {
		c := r.Class
		e.Envelope.Class.Category = c.Category
		e.Envelope.Class.Severity = c.Severity
		e.Envelope.Class.Sensitivity = c.Sensitivity
		e.Envelope.Class.PII = c.PII
		e.Envelope.Class.Sanitized = c.Sanitized
		e.Envelope.Class.Redactions = int(c.Redactions)
		e.Envelope.Class.Labels = slices.Clone(c.Labels)
	}
}

type pipelineError string

func (e pipelineError) Error() string              { return string(e) }
func (e pipelineError) SanitizeErrorClass() string { return string(e) }

func priorFailedClosed(raw string) bool {
	var payload struct {
		Class string `json:"sanitize_error"`
	}
	if json.Unmarshal([]byte(raw), &payload) != nil {
		return false
	}
	switch payload.Class {
	case "panic", "oversized", "invalid_json", "opaque", "depth", "tombstone_with_payload", "redactions_overflow", "unknown_kind", "error", "stage_error":
	default:
		return false
	}
	var compact bytes.Buffer
	if json.Compact(&compact, []byte(raw)) != nil {
		return false
	}
	return bytes.Equal(compact.Bytes(), eventstage.ErrorPayload(payload.Class))
}
