package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeGhostReaper struct{ apply bool }

type badRequest struct{ error }

func (badRequest) BadRequest() bool { return true }

func (f *fakeGhostReaper) ReapGhostsReport(_ context.Context, apply bool, backup string) (any, error) {
	f.apply = apply
	if apply && backup == "" {
		return nil, badRequest{errors.New("backup required")}
	}
	return map[string]any{"applied": apply}, nil
}

// CW-20261114-0007: POST /ghosts/reap is a dry run without a body, and a caller mistake is a 400.
func TestGhostReapRoute(t *testing.T) {
	fake := &fakeGhostReaper{}
	h := (&Server{GhostReaper: fake}).Handler()
	call := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/ghosts/reap", strings.NewReader(body)))
		return w
	}
	w := call("")
	var out map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || out["applied"] != false || fake.apply {
		t.Fatalf("dry run: %d %s", w.Code, w.Body)
	}
	if w = call(`{"apply":true}`); w.Code != 400 {
		t.Fatalf("apply without backup: %d %s", w.Code, w.Body)
	}
	if w = call(`{"apply":true,"backup":"/b"}`); w.Code != 200 || !fake.apply {
		t.Fatalf("apply: %d %s", w.Code, w.Body)
	}
}
