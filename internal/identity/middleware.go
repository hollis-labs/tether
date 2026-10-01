package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

type Mode string

const (
	Off     Mode = "off"
	Observe Mode = "observe"
	Enforce Mode = "enforce"
)

func (m Mode) Validate() error {
	switch m {
	case "", Off, Observe, Enforce:
		return nil
	default:
		return fmt.Errorf("invalid identity.mode (want off, observe or enforce)")
	}
}

type Observation struct {
	At             time.Time `json:"at"`
	PrincipalID    string    `json:"principal_id,omitempty"`
	SessionID      string    `json:"session_id,omitempty"`
	Mode           Mode      `json:"mode"`
	Authentication string    `json:"authentication"`
	Method         string    `json:"method"`
	Route          string    `json:"route"`
}

type Verifier interface {
	Verify(context.Context, string) (Principal, error)
}

// Middleware carries verified identity in context. Observe never rejects a
// request, including invalid credentials and storage errors. Health is open.
// Record receives metadata only, never bearer headers, query strings or bodies.
func Middleware(mode Mode, verifier Verifier, record func(context.Context, Observation) error, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := mode.Validate(); err != nil {
			authError(w, http.StatusServiceUnavailable, "identity_unavailable")
			return
		}
		if mode == Off || mode == "" || r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}
		o := Observation{At: time.Now().UTC(), Mode: mode, Authentication: "missing", Method: r.Method, Route: routeFamily(r.URL.Path)}
		p, state := authenticate(r.Context(), r.Header.Values("Authorization"), verifier)
		o.Authentication = state
		if state == "verified" {
			o.PrincipalID, o.SessionID = p.ID, p.SessionID
			r = r.WithContext(WithPrincipal(r.Context(), p))
		}
		if record != nil {
			if err := record(r.Context(), o); err != nil {
				// Do not print database errors: a driver may echo parameters.
				log.Printf("identity: observation could not be persisted")
				if mode == Enforce {
					authError(w, http.StatusServiceUnavailable, "identity_unavailable")
					return
				}
			}
		}
		if state != "verified" && mode == Enforce {
			status := http.StatusUnauthorized
			code := "unauthorized"
			if state == "unavailable" {
				status, code = http.StatusServiceUnavailable, "identity_unavailable"
			}
			authError(w, status, code)
			return
		}
		if state == "invalid" {
			log.Printf("identity: invalid credential observed")
		}
		if state == "unavailable" {
			log.Printf("identity: verifier unavailable")
		}
		next.ServeHTTP(w, r)
	})
}

func authenticate(ctx context.Context, headers []string, verifier Verifier) (Principal, string) {
	if len(headers) == 0 {
		return Principal{}, "missing"
	}
	if len(headers) != 1 || len(headers[0]) > 256 {
		return Principal{}, "invalid"
	}
	parts := strings.Fields(headers[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return Principal{}, "invalid"
	}
	if verifier == nil {
		return Principal{}, "unavailable"
	}
	p, err := verifier.Verify(ctx, parts[1])
	if errors.Is(err, ErrInvalidToken) {
		return Principal{}, "invalid"
	}
	if err != nil {
		return Principal{}, "unavailable"
	}
	return p, "verified"
}

func authError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": "verified caller identity required"}})
}

func routeFamily(path string) string {
	first, _, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	if len(first) > 64 || strings.Contains(first, "tth_") {
		return "/<redacted>"
	}
	return "/" + first
}

// ValidateBind keeps observe/off restricted to local transports. It does not
// enable enforce, nor does enforce establish transport encryption.
func ValidateBind(addr string, mode Mode) error {
	if err := mode.Validate(); err != nil {
		return err
	}
	if !strings.HasPrefix(addr, "tcp:") {
		return nil
	}
	host, _, err := net.SplitHostPort(strings.TrimPrefix(addr, "tcp:"))
	if err != nil {
		return fmt.Errorf("parse TCP listen address: %w", err)
	}
	ip := net.ParseIP(host)
	if mode != Enforce && host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("non-loopback TCP requires identity.mode enforce")
	}
	return nil
}
