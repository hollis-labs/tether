package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/store"
)

type forceStopFake struct {
	fakeLaunchService
	err  error
	got  ForceStopOptions
	id   string
	hits int
}

func (f *forceStopFake) ForceStopSession(_ context.Context, id string, o ForceStopOptions) (ForceStopResult, error) {
	f.hits++
	f.id, f.got = id, o
	return ForceStopResult{SessionID: id, From: "running", To: "killed", Reason: o.Reason}, f.err
}

func forceStop(svc LaunchService, query string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/sessions/s1/stop?"+query, nil))
	return rr
}

func TestForceStopRoute(t *testing.T) {
	f := &forceStopFake{}
	rr := forceStop(f, "force=true&reason=orphan&allow_live=true")
	if rr.Code != 200 || f.id != "s1" || f.got.Reason != "orphan" || !f.got.AllowLive || !strings.Contains(rr.Body.String(), `"killed"`) {
		t.Fatalf("%d %s %+v", rr.Code, rr.Body.String(), f)
	}
	if len(f.stopIDs) != 0 {
		t.Fatal("plain stop must not run on a force close")
	}
}

func TestForceStopErrors(t *testing.T) {
	for _, c := range []struct {
		err  error
		code int
		name string
	}{
		{ErrSessionLive, 409, "session_live"},
		{ErrSessionAlreadyTerminal, 409, CodeConflict},
		{store.ErrSessionNotFound, 404, CodeNotFound},
	} {
		rr := forceStop(&forceStopFake{err: c.err}, "force=true")
		if rr.Code != c.code || decodeErr(t, rr).Error.Code != c.name {
			t.Errorf("%v: %d %s", c.err, rr.Code, rr.Body.String())
		}
	}
}

func TestForceStopUnsupportedAndPlainStopUnchanged(t *testing.T) {
	if rr := forceStop(&fakeLaunchService{}, "force=true"); rr.Code != http.StatusNotImplemented {
		t.Fatalf("code = %d", rr.Code)
	}
	f := &forceStopFake{}
	if rr := forceStop(f, ""); rr.Code != http.StatusNoContent || f.hits != 0 || len(f.stopIDs) != 1 {
		t.Fatalf("plain stop: %d hits=%d", rr.Code, f.hits)
	}
}
