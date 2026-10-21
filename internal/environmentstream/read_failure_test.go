package environmentstream

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
)

type failingReads struct{ err error }

func (f failingReads) EnvironmentEvents(context.Context, int64, int) (store.EnvironmentWindow, error) {
	return store.EnvironmentWindow{}, f.err
}
func (f failingReads) EnvironmentSnapshot(context.Context, string) (store.EnvironmentSnapshot, error) {
	return store.EnvironmentSnapshot{}, f.err
}

// CW-20261021-0002: a failed durable read is classified. A transient one (the
// database lock, the caller's deadline or connection) is a 503 with Retry-After
// and a body that says to retry; anything else stays a 500 with the generic
// body. Neither body carries the driver's message.
func TestSnapshotReadFailureIsClassifiedAndSanitized(t *testing.T) {
	secret := "SELECT secret_column FROM somewhere"
	for name, tc := range map[string]struct {
		err        error
		status     int
		retryAfter string
		body       string
	}{
		"database locked":   {fmt.Errorf("step: database is locked (5) (SQLITE_BUSY) %s", secret), http.StatusServiceUnavailable, "2", "snapshot unavailable: temporarily busy, retry"},
		"busy by code":      {errors.New("sqlite: SQLITE_BUSY"), http.StatusServiceUnavailable, "2", "snapshot unavailable: temporarily busy, retry"},
		"deadline":          {fmt.Errorf("read: %w", context.DeadlineExceeded), http.StatusServiceUnavailable, "2", "snapshot unavailable: temporarily busy, retry"},
		"client went away":  {fmt.Errorf("read: %w", context.Canceled), http.StatusServiceUnavailable, "2", "snapshot unavailable: temporarily busy, retry"},
		"internal":          {errors.New("no such table: " + secret), http.StatusInternalServerError, "", "snapshot unavailable\n"},
		"constraint/driver": {errors.New("sql: converting NULL to string is unsupported"), http.StatusInternalServerError, "", "snapshot unavailable\n"},
	} {
		t.Run(name, func(t *testing.T) {
			s := &Server{EnvironmentID: "env", Store: failingReads{tc.err}}
			w := httptest.NewRecorder()
			s.Snapshot(w, httptest.NewRequest(http.MethodGet, "/environment/snapshot", nil), "")
			if w.Code != tc.status || w.Header().Get("Retry-After") != tc.retryAfter {
				t.Fatalf("status %d Retry-After %q, want %d %q", w.Code, w.Header().Get("Retry-After"), tc.status, tc.retryAfter)
			}
			if !strings.HasPrefix(w.Body.String(), tc.body) {
				t.Fatalf("body %q, want prefix %q", w.Body.String(), tc.body)
			}
			if strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), "SQLITE") || strings.Contains(w.Body.String(), "sqlite") {
				t.Fatalf("body leaks the driver message: %q", w.Body.String())
			}
		})
	}
}

// The per-session snapshot and the stream's tail read are classified the same way.
func TestStreamReadFailuresAreClassified(t *testing.T) {
	busy := errors.New("database is locked")
	s := &Server{EnvironmentID: "env", Store: failingReads{busy}, Bus: &closingBus{live: make(chan events.Event)}}
	for _, session := range []string{"", "one"} {
		w := httptest.NewRecorder()
		s.Stream(w, httptest.NewRequest(http.MethodGet, "/?after_seq=1", nil), session)
		if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "2" {
			t.Fatalf("session %q: %d %q %s", session, w.Code, w.Header().Get("Retry-After"), w.Body)
		}
	}
}

func TestReadFailureClass(t *testing.T) {
	for err, want := range map[error]string{
		context.DeadlineExceeded:                    "timeout",
		context.Canceled:                            "canceled",
		errors.New("database is locked"):            "busy",
		errors.New("x: SQLITE_BUSY_SNAPSHOT"):       "busy",
		errors.New("database table is locked: foo"): "busy",
		errors.New("something else"):                "internal",
	} {
		if got := readFailureClass(err); got != want {
			t.Errorf("%v: %s, want %s", err, got, want)
		}
	}
}
