package api

// Session reference provenance is derived at the authenticated HTTP boundary.

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

func TestAttachSessionRef_RejectsAssertedProxy(t *testing.T) {
	refs := &recordingRefStore{}
	h := NewHandler(Deps{Service: &fakeLaunchService{}, SessionRefs: refs})
	resp, _ := postRef(t, h, `{"kind":"torque_task","ref_id":"CW-1","source":"proxy"}`)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden || len(refs.got) != 0 {
		t.Fatalf("status=%d writes=%d", resp.StatusCode, len(refs.got))
	}
	var out struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Error.Code != "source_asserted" {
		t.Fatalf("error=%+v decode=%v", out, err)
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

// A caller cannot choose any source; unknown fields are ignored as provenance.
func TestAttachSessionRef_IgnoresAuthoredSource(t *testing.T) {
	h := NewHandler(Deps{Service: &fakeLaunchService{}, SessionRefs: realRefStore(t)})

	resp, _ := postRef(t, h, `{"kind":"torque_task","ref_id":"CW-1","source":"vibes"}`)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 with derived source", resp.StatusCode)
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
	if !out.Inserted || out.Ref.Source != store.SourceAPI {
		t.Errorf("response = %+v, want inserted with source=api", out)
	}
}

func TestAttachSessionRef_PreservesParentItemID(t *testing.T) {
	refs := &recordingRefStore{}
	h := NewHandler(Deps{Service: &fakeLaunchService{}, SessionRefs: refs})

	resp, _ := postRef(t, h, `{"kind":"tesseract_revision","ref_id":"01M2REV1","parent_item_id":"01M2ITEM1","source":"agent"}`)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(refs.got) != 1 {
		t.Fatalf("store saw %d writes, want 1", len(refs.got))
	}
	if refs.got[0].ParentItemID != "01M2ITEM1" {
		t.Errorf("store saw parent_item_id = %q, want 01M2ITEM1", refs.got[0].ParentItemID)
	}

	var out SessionRefAttachResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Ref.ParentItemID != "01M2ITEM1" {
		t.Errorf("response parent_item_id = %q, want 01M2ITEM1", out.Ref.ParentItemID)
	}
}

func TestAttachSessionRef_NonObjectJSONPreservesHTTPError(t *testing.T) {
	for _, tc := range []struct{ body, kind string }{{"[1,2]", "array"}, {"5", "number"}, {"true", "bool"}} {
		t.Run(tc.kind, func(t *testing.T) {
			rows := &recordingRefStore{}
			rr := httptest.NewRecorder()
			NewHandler(Deps{SessionRefs: rows, Service: &fakeLaunchService{}}).ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/sessions/s1/refs", bytes.NewBufferString(tc.body)))
			want := `{"error":{"code":"invalid_request","message":"invalid JSON body: json: cannot unmarshal ` + tc.kind + ` into Go value of type api.SessionRefAttachRequest"}}` + "\n"
			if rr.Code != http.StatusBadRequest || rr.Body.String() != want {
				t.Fatalf("status/body = %d / %q, want 400 / %q", rr.Code, rr.Body.String(), want)
			}
			if len(rows.got) != 0 {
				t.Fatal("malformed request persisted a reference")
			}
		})
	}
}
