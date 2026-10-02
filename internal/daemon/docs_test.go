package daemon_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/daemon"
)

func TestDocsHTTPDaemonMountsAndManifestFetch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	docs := (&app.Service{}).Docs()
	handler := (&daemon.Server{Docs: docs}).Handler()
	read := func(target string, status int) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
		if response.Code != status {
			t.Fatalf("%s: %d %s", target, response.Code, response.Body.String())
		}
		return response
	}
	read("/docs/mcp", http.StatusOK)
	response := read("/docs/mcp/connect", http.StatusOK)
	var doc app.DocBody
	if err := json.Unmarshal(response.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Body == "" || len(doc.Files) == 0 {
		t.Fatal("body/manifest missing")
	}
	response = read("/docs/mcp/connect/file?path="+url.QueryEscape(doc.Files[0].Path), http.StatusOK)
	var file app.DocFileBody
	if err := json.Unmarshal(response.Body.Bytes(), &file); err != nil {
		t.Fatal(err)
	}
	if file.Body == "" || file.SHA256 != doc.Files[0].SHA256 {
		t.Fatal("reference missing attribution")
	}
	read("/docs/mcp/missing", http.StatusNotFound)
	read("/docs/mcp/connect/file?path="+url.QueryEscape("/etc/passwd"), http.StatusNotFound)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/docs/mcp", nil))
	var problem api.ErrorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusMethodNotAllowed || problem.Error.Code != api.CodeMethodNotAllowed {
		t.Fatal("docs method error lacks the typed API envelope")
	}
	response = httptest.NewRecorder()
	(&daemon.Server{}).Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/docs/mcp", nil))
	if response.Code != http.StatusNotFound {
		t.Fatal("disabled docs route mounted")
	}
}
