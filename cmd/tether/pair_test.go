package main

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/hollis-labs/tether/internal/config"
	"gopkg.in/yaml.v3"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/identity"
)

func TestPairAndAuthCommandsUseDaemonAPI(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer synthetic-only" {
			t.Error("synthetic operator credential not delivered")
		}
		switch r.URL.Path {
		case "/auth/pair":
			var req api.PairRequest
			if json.NewDecoder(r.Body).Decode(&req) != nil || req.Label != "worker" || req.TTL != "2m0s" || len(req.Scopes) != 2 {
				t.Error("grant command lost constraints")
			}
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(identity.IssuedGrant{Code: "synthetic-one-time-code"})
		case "/auth/devices":
			_ = json.NewEncoder(w).Encode(map[string]any{"devices": []identity.Device{{ID: "msg://device/synthetic", Label: "worker", Scopes: []string{"read"}}}})
		case "/auth/revoke":
			var req api.DeviceRevokeRequest
			if json.NewDecoder(r.Body).Decode(&req) != nil || req.ID != "msg://device/synthetic" {
				t.Error("revoke identity changed")
			}
			_, _ = w.Write([]byte(`{"revoked":true}`))
		default:
			t.Error("unexpected administration route")
		}
	}))
	defer server.Close()
	factory := func() (*deviceAdminClient, error) {
		return &deviceAdminClient{client: server.Client(), baseURL: server.URL, token: "synthetic-only"}, nil
	}
	output := &bytes.Buffer{}
	pair := newPairCommand(factory)
	pair.SetOut(output)
	pair.SetArgs([]string{"--scope", "read,operate", "--ttl", "2m", "--label", "worker"})
	if err := pair.Execute(); err != nil {
		t.Fatal(err)
	}
	if output.String() != "synthetic-one-time-code\n" {
		t.Fatal("one-time code output failed")
	}
	for _, args := range [][]string{{"list"}, {"revoke", "msg://device/synthetic"}} {
		auth := newAuthCommand(factory)
		output.Reset()
		auth.SetOut(output)
		auth.SetArgs(args)
		if err := auth.Execute(); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(output.String(), "synthetic-only") {
			t.Fatal("credential exposed")
		}
	}
	if calls.Load() != 3 {
		t.Fatal("daemon administration flow incomplete")
	}
	// Invalid scopes and lifetimes must fail before credential resolution.
	for _, args := range [][]string{{"--scope", "*"}, {"--scope", "write"}, {"--ttl", "0s"}, {"--ttl", "2h"}} {
		pair := newPairCommand(func() (*deviceAdminClient, error) { t.Fatal("invalid grant read credential"); return nil, nil })
		pair.SilenceUsage = true
		pair.SilenceErrors = true
		pair.SetArgs(args)
		if err := pair.Execute(); err == nil {
			t.Fatal("invalid command accepted")
		}
	}
}

func TestDeviceAdminClientDoesNotEchoErrorBodyOrFollowRedirect(t *testing.T) {
	var leaked atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { leaked.Store(true) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	client := server.Client()
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	admin := &deviceAdminClient{client: client, baseURL: server.URL, token: "synthetic-only"}
	if err := admin.call(context.Background(), "GET", "/auth/devices", nil, nil); err == nil || leaked.Load() {
		t.Fatal("credential redirect accepted")
	}
}

func TestDeviceAdminRefusesTCPBeforeCredentialRead(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	savedCatalog, savedToken := catalogPath, tokenFilePath
	t.Cleanup(func() { catalogPath, tokenFilePath = savedCatalog, savedToken })
	catalogPath = t.TempDir()
	tokenFilePath = filepath.Join(t.TempDir(), "must-not-read")
	global := config.Global{Daemon: config.DaemonConfig{ListenAddr: "tcp:127.0.0.1:7331", ShutdownTimeout: "1s"}}
	data, err := yaml.Marshal(global)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(catalogPath, "global.yaml"), data, 0600); err != nil {
		t.Fatal(err)
	}
	_, err = localDeviceAdminClient()
	if err == nil || err.Error() != "device administration requires the local Unix socket" {
		t.Fatal("TCP credential custody boundary missed", err)
	}
}
