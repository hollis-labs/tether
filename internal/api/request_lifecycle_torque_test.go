package api

// Optional cross-repository contract test. Point only at an isolated Torque
// httptest server; the normal suite uses the local transport failure fixtures.
import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/requestflow"
	"github.com/hollis-labs/tether/internal/store"
)

func TestRequestLifecycleTorqueContract(t *testing.T) {
	base := os.Getenv("TETHER_TEST_TORQUE_URL")
	if base == "" {
		t.Skip("requires an isolated Torque HTTP test server")
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "contract.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	backend := &requestflow.HTTPBackend{BaseURL: base}
	cat := &config.Catalog{Agents: map[string]config.Agent{"archie": {ID: "archie", URN: archieURN}}}
	srv := httptest.NewServer(NewHandler(Deps{Requests: backend, Service: &fakeLaunchService{}, MessageStore: db.MessagingStore(), Seats: db, Catalog: fixedCatalog{cat}}))
	defer srv.Close()
	h := &typedHarness{srv: srv, db: db}
	call := func(method, path, body string, status int) map[string]any {
		t.Helper()
		req, err := http.NewRequest(method, base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != status {
			t.Fatalf("%s %s => %d %+v", method, path, resp.StatusCode, out)
		}
		return out
	}
	code, out, raw := postRequest(t, h, identity.OperatorID, "cross-repo-submit", requestBody)
	if code != 201 || out.Envelope.TaskID == "" {
		t.Fatalf("real submit %d %s", code, raw)
	}
	code, retry, raw := postRequest(t, h, identity.OperatorID, "cross-repo-submit", requestBody)
	if code != 201 || retry.IDs[0] != out.IDs[0] {
		t.Fatalf("real retry %d %s", code, raw)
	}
	path := "/api/v1/tasks/" + out.Envelope.TaskID
	row := call("GET", path+"/request", "", 200)
	if row["state"] != "submitted" || row["mail_message_id"] != out.IDs[0] || row["alerted_at"] == nil {
		t.Fatalf("offline request %+v", row)
	}
	call("POST", path+"/comments", `{"author":"`+archieURN+`","content":"STATUS started · contract test"}`, 201)
	row = call("GET", path+"/request", "", 200)
	if row["state"] != "acknowledged" || row["ack_at"] == nil {
		t.Fatalf("ack %+v", row)
	}
	if err := backend.Transition(t.Context(), out.Envelope.TaskID, requestflow.Transition{ActorURN: archieURN, State: "in_progress"}); err != nil {
		t.Fatal(err)
	}
	reply := `{"kind":"message","in_reply_to":"` + out.IDs[0] + `","status":"done","body":"Contract complete","evidence":["https://example.test/evidence"]}`
	code, _, raw = postRequest(t, h, archieURN, "cross-repo-final", reply)
	if code != 201 {
		t.Fatalf("real reply %d %s", code, raw)
	}
	row = call("GET", path+"/request", "", 200)
	if row["state"] != "done" || row["closed_at"] == nil {
		t.Fatalf("closed %+v", row)
	}
	task := call("GET", path, "", 200)
	metadata, ok := task["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("task metadata %+v", task)
	}
	marker, ok := metadata["request"].(map[string]any)
	if !ok || marker["state"] != "done" {
		t.Fatalf("task marker %+v", metadata)
	}
}
