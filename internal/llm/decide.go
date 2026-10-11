package llm

import (
	"errors"
	"fmt"
	"strings"
)

// QuestionType is one of the three provider-neutral judgment shapes.
type QuestionType string

const (
	QuestionNoul   QuestionType = "noul"   // yes/no: the probability a statement is true
	QuestionChoice QuestionType = "choice" // pick one of a fixed, ordered set
	QuestionScore  QuestionType = "score"  // place on ordered levels
)

// Score level bounds a decision provider must accept.
const (
	MinScoreLevels = 2
	MaxScoreLevels = 10
)

// OnError is the caller-declared failure mode of a decision. The gateway does
// not act on it: it is carried on every DecisionError so the caller's own code
// path (and the audit trail) can see which mode the caller asked for.
type OnError string

const (
	OnErrorFailOpen   OnError = "fail_open"
	OnErrorFailClosed OnError = "fail_closed"
)

// DecisionOption is one choice of a choice question.
type DecisionOption struct {
	Key         string `json:"key"`
	Description string `json:"description,omitempty"`
}

// DecisionQuestion is one judgment, in provider-neutral form. Options and
// Levels are ordered: order matters to the model and is preserved on the wire.
type DecisionQuestion struct {
	Key          string           `json:"key"`
	Type         QuestionType     `json:"type"`
	Instructions string           `json:"instructions"`
	Options      []DecisionOption `json:"options,omitempty"` // choice
	Levels       []string         `json:"levels,omitempty"`  // score, lowest first
	True         string           `json:"true,omitempty"`    // noul: what yes means
	False        string           `json:"false,omitempty"`   // noul: what no means
}

// DecisionRequest asks a set of independent questions about one state.
type DecisionRequest struct {
	State     any                `json:"state"`
	Questions []DecisionQuestion `json:"questions"`
	// Purpose tags the call for spend accounting and per-purpose budgets.
	Purpose string  `json:"purpose,omitempty"`
	OnError OnError `json:"on_error,omitempty"`
}

// DecisionAnswer is the answer to one question.
type DecisionAnswer struct {
	Type          QuestionType       `json:"type"`
	Choice        string             `json:"choice,omitempty"`        // choice: most likely option key
	Noul          float64            `json:"noul,omitempty"`          // noul: probability of yes
	Score         float64            `json:"score,omitempty"`         // score: probability-weighted level index
	Probabilities map[string]float64 `json:"probabilities,omitempty"` // choice: by option key; score: by level index
	// Confidence is nil when the provider reports none (JEV reports none for
	// noul); callers derive their own.
	Confidence *float64 `json:"confidence,omitempty"`
}

// DecisionResult carries the answers by question key.
type DecisionResult struct {
	Answers   map[string]DecisionAnswer `json:"answers"`
	LatencyMS int64                     `json:"latency_ms,omitempty"`
	RequestID string                    `json:"request_id,omitempty"`
}

// ErrInvalidDecision marks a decision request that fails validation.
var ErrInvalidDecision = errors.New("invalid decision request")

// Validate checks the provider-neutral shape of a decision request.
func (d *DecisionRequest) Validate() error {
	if d == nil {
		return fmt.Errorf("%w: decision is required", ErrInvalidDecision)
	}
	if d.State == nil {
		return fmt.Errorf("%w: state is required", ErrInvalidDecision)
	}
	if s, ok := d.State.(string); ok && strings.TrimSpace(s) == "" {
		return fmt.Errorf("%w: state is empty", ErrInvalidDecision)
	}
	switch d.OnError {
	case "", OnErrorFailOpen, OnErrorFailClosed:
	default:
		return fmt.Errorf("%w: on_error must be fail_open or fail_closed", ErrInvalidDecision)
	}
	if len(d.Questions) == 0 {
		return fmt.Errorf("%w: at least one question is required", ErrInvalidDecision)
	}
	seen := map[string]struct{}{}
	for i, q := range d.Questions {
		if q.Key == "" {
			return fmt.Errorf("%w: questions[%d] missing key", ErrInvalidDecision, i)
		}
		if _, dup := seen[q.Key]; dup {
			return fmt.Errorf("%w: duplicate question key %q", ErrInvalidDecision, q.Key)
		}
		seen[q.Key] = struct{}{}
		if strings.TrimSpace(q.Instructions) == "" {
			return fmt.Errorf("%w: question %q missing instructions", ErrInvalidDecision, q.Key)
		}
		switch q.Type {
		case QuestionNoul:
		case QuestionChoice:
			if len(q.Options) < 2 {
				return fmt.Errorf("%w: choice question %q needs at least 2 options", ErrInvalidDecision, q.Key)
			}
			opts := map[string]struct{}{}
			for _, o := range q.Options {
				if o.Key == "" {
					return fmt.Errorf("%w: choice question %q has an option without a key", ErrInvalidDecision, q.Key)
				}
				if _, dup := opts[o.Key]; dup {
					return fmt.Errorf("%w: choice question %q has duplicate option %q", ErrInvalidDecision, q.Key, o.Key)
				}
				opts[o.Key] = struct{}{}
			}
		case QuestionScore:
			if n := len(q.Levels); n < MinScoreLevels || n > MaxScoreLevels {
				return fmt.Errorf("%w: score question %q needs %d..%d levels, has %d", ErrInvalidDecision, q.Key, MinScoreLevels, MaxScoreLevels, n)
			}
		default:
			return fmt.Errorf("%w: question %q has unsupported type %q", ErrInvalidDecision, q.Key, q.Type)
		}
	}
	return nil
}

// DecisionErrorKind classifies a provider failure for callers.
type DecisionErrorKind string

const (
	DecisionErrAuth        DecisionErrorKind = "auth"         // credentials rejected
	DecisionErrInvalid     DecisionErrorKind = "invalid"      // provider rejected the request; never retried
	DecisionErrRateLimited DecisionErrorKind = "rate_limited" // retries exhausted
	DecisionErrOverloaded  DecisionErrorKind = "overloaded"   // retries exhausted
	DecisionErrTimeout     DecisionErrorKind = "timeout"
	DecisionErrCircuitOpen DecisionErrorKind = "circuit_open"
	DecisionErrUnavailable DecisionErrorKind = "unavailable" // transport or unexpected response
)

// DecisionError is the typed failure of a decision call. It carries the
// caller's declared OnError so the mode survives the trip back to the caller.
// Message never contains credentials.
type DecisionError struct {
	Kind    DecisionErrorKind `json:"kind"`
	Status  int               `json:"status,omitempty"`
	OnError OnError           `json:"on_error,omitempty"`
	Message string            `json:"message,omitempty"`
}

func (e *DecisionError) Error() string {
	msg := "decision " + string(e.Kind)
	if e.Status != 0 {
		msg += fmt.Sprintf(" (http %d)", e.Status)
	}
	if e.OnError != "" {
		msg += " [" + string(e.OnError) + "]"
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}
