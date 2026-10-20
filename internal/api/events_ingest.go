package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents/ingest"

	"github.com/hollis-labs/tether/internal/eventingest"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/identity"
)

// EventIngestService appends published events (POST /events).
type EventIngestService interface {
	Ingest(ctx context.Context, c eventingest.Caller, items []json.RawMessage) ([]ingest.Result, error)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		if s.EventIngest == nil {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		s.handleEventsIngest(w, r)
		return
	}
	if s.EventsStore == nil {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	s.handleEventsList(w, r)
}

// handleEventsIngest serves POST /events. The body is one runtimeevents
// envelope or {"events":[...]}; the answer is 200 with one result per event.
// Whole-request failures use the HTTP status (see the libs ingest package).
func (s *Server) handleEventsIngest(w http.ResponseWriter, r *http.Request) {
	caller, status, code, msg := s.ingestCaller(r)
	if status != 0 {
		writeError(w, status, code, msg)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, ingest.MaxRequestBytes+1))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", ingest.ErrRequestTooLarge.Error())
			return
		}
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "read request body")
		return
	}
	items, err := ingest.ParseRequest(body)
	switch {
	case errors.Is(err, ingest.ErrRequestTooLarge), errors.Is(err, ingest.ErrBatchTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", err.Error())
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
		return
	}
	results, err := s.EventIngest.Ingest(r.Context(), caller, items)
	var limited *eventingest.RateLimited
	switch {
	case errors.Is(err, eventingest.ErrAppMismatch):
		writeError(w, http.StatusForbidden, "app_mismatch", "an event names an app the credential is not bound to")
		return
	case errors.As(err, &limited):
		secs := int(limited.RetryAfter.Seconds() + 0.999)
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		writeError(w, http.StatusTooManyRequests, "rate_limited", "event rate limit exceeded")
		return
	case err != nil:
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "events_unavailable", "event log unavailable; retry the request")
		return
	}
	writeJSON(w, http.StatusOK, ingest.NewResponse(results))
}

// ingestCaller decides who may publish and as which app. A non-zero status is
// the refusal to send.
//
// The publish scope (tether.events) follows the identity mode of the local
// API: enforce refuses a caller without it, observe allows and logs, off
// allows. An issued session (agent) token never publishes in enforce mode.
// A service credential is bound to its app in every mode where it verified.
func (s *Server) ingestCaller(r *http.Request) (c eventingest.Caller, status int, code, msg string) {
	p, has := identity.FromContext(r.Context())
	verified := has && p.Verified
	mode := s.EventIngestMode.Canonical()
	c.Key = "anonymous"
	c.Actor = events.Actor{Kind: "service", URN: "msg://service/unverified/ingest", Verified: "none", Role: "system"}
	if verified {
		c.Key = p.ID
		c.Actor = ingestActor(p)
		granted := identity.ScopeGranted(p.Scopes, identity.ScopeEvents)
		switch mode {
		case identity.Enforce:
			if p.Kind == "session" || p.Kind == "agent" {
				return c, http.StatusForbidden, "scope_required", "agent credentials cannot publish events"
			}
			if !granted {
				return c, http.StatusForbidden, "scope_required", "required scope: tether.events"
			}
		default:
			if !granted || p.Kind == "session" || p.Kind == "agent" {
				log.Printf("events ingest: would deny %s principal %q (needs tether.events)", p.Kind, p.ID)
			}
		}
		switch p.Kind {
		case "service":
			app, ok := serviceApp(p.ID)
			if !ok {
				return c, http.StatusForbidden, "app_mismatch", "service credential has no app"
			}
			c.App = app
		}
		// A paired worker device is a host credential (DEV decision): it is
		// not bound to one app and may publish as any registered app, so the
		// services on a host can relay through it. Kind ownership is still
		// checked per event app and the actor is the device. Release posture
		// needs a per-device app allowlist stored with the grant (CW-20261020-0031).
		return c, 0, "", ""
	}
	if mode == identity.Enforce {
		return c, http.StatusUnauthorized, "unauthorized", "a verified credential with tether.events is required"
	}
	return c, 0, "", ""
}

func ingestActor(p identity.Principal) events.Actor {
	a := events.Actor{URN: p.ID, Verified: "token", Role: "system"}
	switch p.Kind {
	case "operator":
		a.Kind, a.Role = "user", "operator"
	case "device":
		a.Kind = "device"
	default:
		a.Kind = "service"
	}
	return a
}

// serviceApp returns <app> from a service principal id msg://service/<app>/<env>.
func serviceApp(id string) (string, bool) {
	rest, ok := strings.CutPrefix(id, "msg://service/")
	if !ok {
		return "", false
	}
	app, _, _ := strings.Cut(rest, "/")
	return app, app != ""
}
