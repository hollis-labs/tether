// Package eventingest is the hub side of POST /events (CW-20261020-0010): apps
// publish runtimeevents v1.1 envelopes, and the hub validates, binds, sanitizes,
// dedupes and appends them to the one event log. The wire contract (limits,
// per-item results, reject codes, kind ownership) is the libs ingest package.
package eventingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents"
	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents/ingest"
	"github.com/hollis-labs/libs/security/redact"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
)

// ErrAppMismatch means a credential bound to one app published an event that
// names another. The whole request is refused (HTTP 403).
var ErrAppMismatch = errors.New("eventingest: event app does not match the credential's app")

// ErrUnavailable means the log could not be written. The producer retries the
// request; ids make that safe.
var ErrUnavailable = errors.New("eventingest: event log unavailable")

// Bus is the event log's writer: the ingest writes on the store's transaction
// and the bus delivers after the commit.
type Bus = events.OutboxBus

// Store is the ingest's view of the event store.
type Store interface {
	// IngestEvent commits the event and its digest together.
	IngestEvent(ctx context.Context, bus events.OutboxBus, e events.Event, digest, app string, at time.Time) (events.Event, store.IngestOutcome, error)
}

// Caller is who is publishing. The API layer builds it from the verified
// credential and the auth mode; the service never looks at credentials.
type Caller struct {
	// App is the app the credential is bound to. Empty means any app (the
	// local operator, or auth off).
	App string
	// Actor is stamped on every event the caller publishes, overwriting
	// whatever the producer sent.
	Actor events.Actor
	// Key identifies the caller for rate limiting.
	Key string
}

// Config tunes a Service.
type Config struct {
	// AllowUnknownKinds accepts kinds missing from the catalog (shadow mode).
	AllowUnknownKinds bool
	// RatePerSecond and Burst limit events per caller key. Zero disables.
	RatePerSecond float64
	Burst         int
	// Now is the clock, for tests.
	Now func() time.Time
}

// Service ingests events.
type Service struct {
	bus       Bus
	store     Store
	cfg       Config
	sanitizer *redact.Sanitizer

	mu      sync.Mutex // serializes check, publish and record so a retry sees the first write
	limiter *limiter
}

// New returns a Service.
func New(bus Bus, store Store, cfg Config) (*Service, error) {
	if bus == nil || store == nil {
		return nil, errors.New("eventingest: bus and store are required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	s, err := redact.NewSanitizer(redact.Policy{Mode: redact.Denylist, Sensitivity: redact.Private, MaxBytes: ingest.MaxPayloadBytes})
	if err != nil {
		return nil, err
	}
	svc := &Service{bus: bus, store: store, cfg: cfg, sanitizer: s}
	if cfg.RatePerSecond > 0 && cfg.Burst > 0 {
		svc.limiter = newLimiter(cfg.RatePerSecond, cfg.Burst, cfg.Now)
	}
	return svc, nil
}

// warm runs the ingest check once on an empty event, which compiles the schema.
// The event is rejected as expected; only an unavailable schema is an error.
func warm() error {
	_, rej := ingest.CheckItem("warmup", json.RawMessage(`{}`), ingest.Options{})
	if rej == nil || strings.Contains(rej.Message, "schema unavailable") {
		return errors.New("eventingest: ingest schema unavailable")
	}
	return nil
}

// RateLimited carries the wait a producer should observe before retrying.
type RateLimited struct{ RetryAfter time.Duration }

func (r *RateLimited) Error() string { return "eventingest: rate limited" }

// Ingest checks every item and appends the accepted ones. Results are in
// request order. ErrAppMismatch is returned before anything is written.
func (s *Service) Ingest(ctx context.Context, c Caller, items []json.RawMessage) ([]ingest.Result, error) {
	heads := make([]head, len(items))
	for i, raw := range items {
		_ = json.Unmarshal(raw, &heads[i]) // an undecodable item is rejected below
		if c.App != "" && heads[i].App != "" && heads[i].App != c.App {
			return nil, ErrAppMismatch
		}
	}
	if s.limiter != nil {
		if wait := s.limiter.take(c.Key, len(items)); wait > 0 {
			return nil, &RateLimited{RetryAfter: wait}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	results := make([]ingest.Result, 0, len(items))
	for i, raw := range items {
		r, err := s.one(ctx, c, heads[i], raw)
		if err != nil {
			return results, fmt.Errorf("%w: %w", ErrUnavailable, err)
		}
		results = append(results, r)
	}
	return results, nil
}

type head struct {
	ID  string `json:"id"`
	App string `json:"app"`
}

func (s *Service) one(ctx context.Context, c Caller, h head, raw json.RawMessage) (ingest.Result, error) {
	app := c.App
	if app == "" {
		app = h.App
	}
	if app == "" {
		rej := &ingest.Rejection{Code: ingest.CodeSchemaInvalid, Message: "app is required at ingest"}
		return rej.Result(h.ID), nil
	}
	ev, rej := ingest.CheckItem(app, raw, ingest.Options{AllowUnknownKinds: s.cfg.AllowUnknownKinds})
	if rej != nil {
		return rej.Result(h.ID), nil
	}
	if ev.App == "" {
		rej := &ingest.Rejection{Code: ingest.CodeSchemaInvalid, Message: "app is required at ingest"}
		return rej.Result(ev.ID), nil
	}
	digest, err := eventDigest(raw)
	if err != nil {
		rej := &ingest.Rejection{Code: ingest.CodeSchemaInvalid, Message: "event cannot be canonicalized"}
		return rej.Result(ev.ID), nil
	}

	stored, outcome, err := s.append(ctx, c, ev, digest)
	if err != nil {
		return ingest.Result{}, err
	}
	switch outcome {
	case store.IngestUnverifiable:
		// Stored, but with no digest on record: never acknowledged as a duplicate.
		log.Printf("eventingest: event %s is stored without an ingest digest; refusing the id (not a duplicate)", ev.ID)
		rej := &ingest.Rejection{Code: ingest.CodeIDConflict, Message: "id is stored without verifiable evidence; not acknowledged"}
		return rej.Result(ev.ID), nil
	case store.IngestConflict:
		rej := &ingest.Rejection{Code: ingest.CodeIDConflict, Message: "id is already stored with a different event"}
		return rej.Result(ev.ID), nil
	case store.IngestDuplicate:
		return ingest.Result{ID: ev.ID, Status: ingest.StatusDuplicate, Cursor: cursor(stored.Seq)}, nil
	}
	return ingest.Result{ID: ev.ID, Status: ingest.StatusAccepted, Cursor: cursor(stored.Seq)}, nil
}

func (s *Service) append(ctx context.Context, c Caller, ev runtimeevents.Event, digest string) (events.Event, store.IngestOutcome, error) {
	payload, class := s.sanitize(ev)
	e := toEvent(ev, c.Actor, payload, class)
	return s.store.IngestEvent(ctx, s.bus, e, digest, ev.App, s.cfg.Now())
}

// sanitize is the hub's backstop: the producer sanitizes first, and the hub
// scrubs again with the shared redaction engine. It fails closed: a payload
// that cannot be sanitized is replaced by {"sanitize_error": class} and the
// event is stored with sanitized=false.
func (s *Service) sanitize(ev runtimeevents.Event) (string, events.Class) {
	class := events.Class{Sanitized: true}
	if len(ev.Payload) == 0 || string(ev.Payload) == "null" {
		return "", class
	}
	out, rep := s.sanitizer.Sanitize(ev.Payload)
	if rep.ErrorClass != "" {
		b, _ := json.Marshal(map[string]string{"sanitize_error": string(rep.ErrorClass)})
		return string(b), events.Class{Sanitized: false}
	}
	class.Redactions = int(rep.Redactions)
	// The log never stores a secret-class event. What the engine found was
	// removed from out, so what is stored is private.
	class.Sensitivity = "private"
	if rep.Sensitivity == redact.Public && rep.Redactions == 0 {
		class.Sensitivity = "public"
	}
	class.PII = rep.PII
	return string(out), class
}

func toEvent(ev runtimeevents.Event, actor events.Actor, payload string, hub events.Class) events.Event {
	if ev.Class != nil && strings.EqualFold(ev.Class.Sensitivity, "secret") {
		// A producer that calls its own payload secret gets it withheld: the
		// log does not store secrets, and a refusal here would be retried forever.
		payload = `{"withheld":"secret"}`
		hub.Sensitivity, hub.Sanitized = "private", true
	}
	scope := events.ScopeDaemon
	if ev.SessionID != "" {
		scope = events.ScopeSession
	}
	env := events.Envelope{
		SchemaVersion: runtimeevents.SchemaVersion,
		App:           ev.App,
		Subject:       ev.Subject,
		TurnID:        ev.TurnID,
		ParentID:      ev.ParentID,
		RawOffset:     ev.RawOffset,
		Process: events.Process{PID: ev.Process.PID, Provider: ev.Process.Provider, Runtime: ev.Process.Runtime,
			ProviderSessionID: ev.Process.ProviderSessionID},
		Source:      events.Source{Channel: string(ev.Source.Channel), Confidence: string(ev.Source.Confidence)},
		Sequence:    ev.Sequence,
		Generation:  ev.Generation,
		ContentType: ev.ContentType,
		Truncated:   ev.Truncated,
		Actor:       actor,
		Class:       hubClass(ev, hub),
	}
	if ev.Scope != nil {
		// environment, authority, worker and host are the hub's to stamp.
		env.Dims = events.ScopeDims{
			ProjectID: ev.Scope.ProjectID, WorkstreamID: ev.Scope.WorkstreamID, EpicID: ev.Scope.EpicID,
			SprintID: ev.Scope.SprintID, PlanID: ev.Scope.PlanID, PhaseID: ev.Scope.PhaseID, TaskID: ev.Scope.TaskID,
			RunID: ev.Scope.RunID, Attempt: int(ev.Scope.Attempt), LaunchID: ev.Scope.LaunchID, AgentURN: ev.Scope.AgentURN,
			LogicalAgentID: ev.Scope.LogicalAgentID, TeamRunID: ev.Scope.TeamRunID,
		}
	}
	if ev.Corr != nil {
		env.Corr = events.Corr{TraceID: ev.Corr.TraceID, SpanID: ev.Corr.SpanID, Traceparent: ev.Corr.Traceparent,
			InReplyTo: ev.Corr.InReplyTo, CausationID: ev.Corr.CausationID, CorrelationID: ev.Corr.CorrelationID,
			ThreadID: ev.Corr.ThreadID, IdempotencyKey: ev.Corr.IdempotencyKey, Depth: int(ev.Corr.Depth)}
	}
	return events.Event{
		EventID:        ev.ID,
		At:             ev.Time.UTC(),
		Scope:          scope,
		SessionID:      ev.SessionID,
		LogicalAgentID: env.Dims.LogicalAgentID,
		Envelope:       env,
		Kind:           string(ev.Kind),
		PayloadJSON:    payload,
	}
}

// hubClass keeps what the producer may say about the event (category,
// severity, payload schema, labels, a narrower visibility) and takes the rest
// from the hub's own sanitize result.
func hubClass(ev runtimeevents.Event, hub events.Class) events.Class {
	out := hub
	out.Visibility = "environment"
	if ev.Class == nil {
		return out
	}
	out.Category, out.Severity, out.PayloadSchema = ev.Class.Category, ev.Class.Severity, ev.Class.PayloadSchema
	out.Labels = append([]string(nil), ev.Class.Labels...)
	// A producer may narrow visibility, never widen it: mesh and local are not
	// accepted from ingest (local is rejected earlier; mesh is catalog-only).
	if ev.Class.Visibility == "operator" {
		out.Visibility = "operator"
	}
	// The producer's own sensitivity claim can only raise the hub's, up to
	// private: a secret claim is handled in toEvent.
	if ev.Class.Sensitivity == "private" && sensitivityRank(out.Sensitivity) < sensitivityRank("private") {
		out.Sensitivity = "private"
	}
	out.PII = out.PII || ev.Class.PII
	return out
}

func sensitivityRank(s string) int {
	switch strings.ToLower(s) {
	case "secret":
		return 3
	case "private":
		return 2
	case "public":
		return 1
	}
	return 0
}

func cursor(seq int64) string { return strconv.FormatInt(seq, 10) }
