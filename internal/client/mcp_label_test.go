package client

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

type recordingRT struct{ seen *http.Request }

func (r *recordingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	r.seen = req
	return &http.Response{StatusCode: 200, Body: http.NoBody, Header: http.Header{}}, nil
}

func TestMCPTransportSendsClientLabelWithoutMutatingTheCallersRequest(t *testing.T) {
	base := &recordingRT{}
	rt := mcpErrorTransport{base: base, clientLabel: "tether-sysadmin"}
	orig := httptest.NewRequest(http.MethodPost, "http://unix/mcp", nil)
	if _, err := rt.RoundTrip(orig); err != nil {
		t.Fatal(err)
	}
	if got := base.seen.Header.Get("X-Tether-Client-Label"); got != "tether-sysadmin" {
		t.Fatalf("header = %q", got)
	}
	if orig.Header.Get("X-Tether-Client-Label") != "" {
		t.Fatal("RoundTrip mutated the caller's request")
	}
	// No label configured: no header at all.
	base2 := &recordingRT{}
	if _, err := (mcpErrorTransport{base: base2}).RoundTrip(httptest.NewRequest(http.MethodPost, "http://unix/mcp", nil)); err != nil {
		t.Fatal(err)
	}
	if _, present := base2.seen.Header["X-Tether-Client-Label"]; present {
		t.Fatal("unlabeled client must not send the header")
	}
}
