package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDiscoverModels(t *testing.T) {
	t.Parallel()

	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[
			{"id":"vendor/a","object":"model"},
			{"id":"  "},
			{"id":"vendor/b"},
			{"id":"vendor/a"}
		]}`))
	}))
	t.Cleanup(srv.Close)

	ids, err := DiscoverModels(context.Background(), DiscoveryConfig{BaseURL: srv.URL + "/v1/", APIKey: "sk-test"})
	if err != nil {
		t.Fatalf("DiscoverModels: %v", err)
	}
	if want := []string{"vendor/a", "vendor/b"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	if gotPath != "/v1/models" {
		t.Errorf("path = %q, want /v1/models", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want bearer token", gotAuth)
	}
}

func TestDiscoverModelsNoKeySendsNoAuthorization(t *testing.T) {
	t.Parallel()

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":[{"id":"m"}]}`))
	}))
	t.Cleanup(srv.Close)

	if _, err := DiscoverModels(context.Background(), DiscoveryConfig{BaseURL: srv.URL}); err != nil {
		t.Fatalf("DiscoverModels: %v", err)
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want none", gotAuth)
	}
}

func TestDiscoverModelsEmptyList(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	t.Cleanup(srv.Close)

	ids, err := DiscoverModels(context.Background(), DiscoveryConfig{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("DiscoverModels: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("ids = %v, want empty", ids)
	}
}

func TestDiscoverModelsErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		handler http.HandlerFunc
		wantErr string
	}{
		{
			name:    "404",
			handler: func(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) },
			wantErr: "unexpected status 404",
		},
		{
			name: "malformed body",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"data":[{"id":`))
			},
			wantErr: "decode response",
		},
		{
			name: "html body",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`<html>not json</html>`))
			},
			wantErr: "decode response",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(tc.handler)
			t.Cleanup(srv.Close)

			ids, err := DiscoverModels(context.Background(), DiscoveryConfig{BaseURL: srv.URL})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
			if ids != nil {
				t.Fatalf("ids = %v, want nil on error", ids)
			}
		})
	}
}

func TestDiscoverModelsTimeout(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	start := time.Now()
	_, err := DiscoverModels(context.Background(), DiscoveryConfig{BaseURL: srv.URL, Timeout: 50 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("discovery took %s, want it bounded by the timeout", elapsed)
	}
}
