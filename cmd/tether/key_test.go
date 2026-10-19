package main

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

type keyInventoryTransport func(*http.Request) (*http.Response, error)

func (f keyInventoryTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestKeyListUsesMetadataAPI(t *testing.T) {
	transport := keyInventoryTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" || r.URL.Path != "/auth/keys" || r.Header.Get("Authorization") != "Bearer synthetic-only" {
			t.Fatal("inventory did not use authenticated daemon GET")
		}
		q := r.URL.Query()
		if q.Get("principal") != "msg://device/a?b&c" || q.Get("kind") != "device" || q.Get("after") != "42" || q.Get("limit") != "10" {
			t.Fatal("inventory query lost constraints or escaped principal")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"credentials":[{"credential_id":43,"principal_id":"msg://device/test","kind":"device","scopes":["read"],"created_at":"2026-01-01T00:00:00Z","token":"must-not-echo","token_hash":"must-not-echo-hash"}],"next_after":43}`)), Header: make(http.Header)}, nil
	})
	factory := func() (*deviceAdminClient, error) {
		return &deviceAdminClient{client: &http.Client{Transport: transport}, baseURL: "http://unix", token: "synthetic-only"}, nil
	}
	cmd := newKeyCommand(factory)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"list", "--kind", "device", "--principal", "msg://device/a?b&c", "--after", "42", "--limit", "10"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"next_after":43`) || !strings.Contains(out.String(), `"principal_id":"msg://device/test"`) {
		t.Fatal("inventory metadata omitted")
	}
	if strings.Contains(out.String(), "must-not-echo") || strings.Contains(out.String(), "synthetic-only") {
		t.Fatal("inventory echoed credential material")
	}
}

func TestKeyListRejectsInvalidQueryBeforeCredentialRead(t *testing.T) {
	for _, args := range [][]string{{"--limit", "0"}, {"--limit", "201"}, {"--after", "-1"}, {"--kind", "unknown"}} {
		cmd := newKeyCommand(func() (*deviceAdminClient, error) {
			t.Fatal("invalid query read credentials")
			return nil, nil
		})
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs(append([]string{"list"}, args...))
		if err := cmd.Execute(); err == nil {
			t.Fatal("invalid query accepted")
		}
	}
}
