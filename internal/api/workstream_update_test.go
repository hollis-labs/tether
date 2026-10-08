package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/hollis-labs/tether/internal/store"
)

func patchWorkstream(t *testing.T, base, id, body string) (int, WorkstreamDTO) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPatch, base+"/workstreams/"+id, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out WorkstreamDTO
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, out
}

func TestWorkstreamUpdate_OverTheWire(t *testing.T) {
	srv, db := newDigestServer(t)
	ws, err := createTestWorkstream(db)
	if err != nil {
		t.Fatal(err)
	}

	code, got := patchWorkstream(t, srv.URL, ws.ID, `{"workflow_id":"torque:PRJ-1","status":"closed"}`)
	if code != http.StatusOK || got.WorkflowID != "torque:PRJ-1" || got.Status != "closed" || got.Name != "keep" {
		t.Fatalf("patch = %d %+v", code, got)
	}
	code, got = patchWorkstream(t, srv.URL, ws.ID, `{"workflow_id":""}`)
	if code != http.StatusOK || got.WorkflowID != "" || got.Name != "keep" {
		t.Fatalf("clear = %d %+v", code, got)
	}
	if code, _ := patchWorkstream(t, srv.URL, "ghost", `{"name":"x"}`); code != http.StatusNotFound {
		t.Errorf("unknown id = %d, want 404", code)
	}
	if code, _ := patchWorkstream(t, srv.URL, ws.ID, `{"status":"bogus"}`); code != http.StatusBadRequest {
		t.Errorf("bad status = %d, want 400", code)
	}
}

func createTestWorkstream(db WorkstreamStore) (WorkstreamDTO, error) {
	row, err := db.CreateWorkstream(store.WorkstreamRow{Name: "keep", WorkflowID: "wf-0"})
	return workstreamToDTO(row), err
}
