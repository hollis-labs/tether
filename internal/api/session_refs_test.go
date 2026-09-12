package api

// session_refs_test.go — CW-20260912-0060 / CW-20260912-0061.
//
// The behavior under test is mostly an absence: this endpoint applies NO
// guard to `source`, including source=proxy. It used to reject that value, and
// the rejection was wrong for two independent reasons documented at the call
// site — it could not detect impersonation under ADR 0045, and it blocked the
// only caller that had a legitimate claim to it, since `mux mcp --proxy` is a
// separate process that reaches the store through this route.
//
// An absence is exactly what regresses silently, so it is asserted.

// NOTE ON THE Deps: every handler below needs Service, not just SessionRefs.
// /sessions/{id}/refs is a sub-action of the /sessions/ route, and
// registerSessionRoutes returns early when Service is nil — so a server
// configured with SessionRefs and no Service serves no refs at all. Latent
// rather than live (the daemon always wires Service), but it is why these
// tests carry a launch-service stub they never exercise.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hollis-labs/tether/internal/store"
)

// recordingRefStore captures what reached the store, so the test can assert on
// the value persisted rather than on the handler's response alone.
type recordingRefStore struct {
	got []store.SessionRefRow
}

func (r *recordingRefStore) AttachSessionRef(ref store.SessionRefRow) (store.AttachRefResult, error) {
	r.got = append(r.got, ref)
	return store.AttachRefResult{Inserted: true}, nil
}

func (r *recordingRefStore) ListSessionRefs(string, store.ListSessionRefsOptions) ([]store.SessionRefRow, error) {
	return nil, nil
}

func (r *recordingRefStore) ListWorkstreamRefs(string, store.ListSessionRefsOptions) ([]store.SessionRefRow, error) {
	return nil, nil
}

func postRef(t *testing.T, h http.Handler, body string) (*http.Response, *recordingRefStore) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/sessions/sess-1/refs", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result(), nil
}

// The proxy must be able to record what it observed. It writes through this
// endpoint because it runs in a separate process from the daemon and cannot
// reach the store directly.
func TestAttachSessionRef_AcceptsSourceProxy(t *testing.T) {
	refs := &recordingRefStore{}
	h := NewHandler(Deps{Service: &fakeLaunchService{}, SessionRefs: refs})

	resp, _ := postRef(t, h, `{"kind":"torque_task","ref_id":"CW-1","relation":"read","source":"proxy"}`)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: the proxy cannot record observations if this endpoint rejects them", resp.StatusCode)
	}
	if len(refs.got) != 1 {
		t.Fatalf("store saw %d writes, want 1", len(refs.got))
	}
	if refs.got[0].Source != store.SourceProxy {
		t.Errorf("persisted source = %q, want %q — the value must reach the store unaltered, or the provenance it records is fiction",
			refs.got[0].Source, store.SourceProxy)
	}
}

// Omitting source still defaults to api, so an ordinary caller is not
// silently recorded as proxy-observed.
func TestAttachSessionRef_DefaultsToAPI(t *testing.T) {
	refs := &recordingRefStore{}
	h := NewHandler(Deps{Service: &fakeLaunchService{}, SessionRefs: refs})

	resp, _ := postRef(t, h, `{"kind":"git_commit","ref_id":"abc1234","relation":"created"}`)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if refs.got[0].Source != store.SourceAPI {
		t.Errorf("default source = %q, want %q", refs.got[0].Source, store.SourceAPI)
	}
}

// The vocabulary is still closed. Relaxing the proxy guard is not a decision
// to accept anything — an unknown source is rejected by the store, and the
// handler surfaces that rather than swallowing it.
func TestAttachSessionRef_RejectsUnknownSource(t *testing.T) {
	h := NewHandler(Deps{Service: &fakeLaunchService{}, SessionRefs: realRefStore(t)})

	resp, _ := postRef(t, h, `{"kind":"torque_task","ref_id":"CW-1","source":"vibes"}`)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an unknown source", resp.StatusCode)
	}
}

// kind and ref_id remain required — the triple is the payload and a ref
// without one is not a ref.
func TestAttachSessionRef_RequiresKindAndRefID(t *testing.T) {
	h := NewHandler(Deps{Service: &fakeLaunchService{}, SessionRefs: &recordingRefStore{}})

	for _, body := range []string{
		`{"ref_id":"CW-1"}`,
		`{"kind":"torque_task"}`,
	} {
		resp, _ := postRef(t, h, body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400", body, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
}

// realRefStore gives the vocabulary test a store that actually validates,
// since the recording stub deliberately accepts everything.
func realRefStore(t *testing.T) SessionRefStore {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/refs-api.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// The response reports what happened, so a hook that runs twice can tell
// "recorded" from "already knew" without re-reading.
func TestAttachSessionRef_ResponseReportsInserted(t *testing.T) {
	h := NewHandler(Deps{Service: &fakeLaunchService{}, SessionRefs: &recordingRefStore{}})

	resp, _ := postRef(t, h, `{"kind":"adr","ref_id":"0046","source":"agent"}`)
	defer func() { _ = resp.Body.Close() }()

	var out SessionRefAttachResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Inserted || out.Ref.Source != store.SourceAgent {
		t.Errorf("response = %+v, want inserted with source=agent", out)
	}
}
