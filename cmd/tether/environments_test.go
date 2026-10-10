package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/client"
	"github.com/spf13/cobra"
)

func TestEnvironmentDirectoryCLIUsesDaemon(t *testing.T) {
	calls := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		if r.Header.Get("Authorization") != "" {
			t.Error("ambient test credential")
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" && r.URL.Path == "/environments" {
			_, _ = w.Write([]byte(`{"environments":[]}`))
			return
		}
		if r.Method == "POST" {
			w.WriteHeader(201)
		}
		_, _ = w.Write([]byte(`{"environmentId":"id","state":"retired","revocationPending":true}`))
	}))
	defer server.Close()
	old := environmentDirectoryClientFactory
	environmentDirectoryClientFactory = func() (*client.Client, error) {
		return client.New("tcp:"+server.Listener.Addr().String(), client.WithToken("")), nil
	}
	t.Cleanup(func() { environmentDirectoryClientFactory = old })
	for _, tc := range []struct {
		args          []string
		input, method string
	}{
		{[]string{"list"}, "", "GET /environments"},
		{[]string{"get", "id"}, "", "GET /environments/id"},
		{[]string{"register"}, `{"environmentId":"expected","authority":"worker","routes":[{"baseURL":"https://worker.example"}],"credentialReference":"file:///synthetic","deviceId":"device","ownership":"external"}`, "POST /environments"},
		{[]string{"rename", "id", "label"}, "", "PATCH /environments/id"},
		{[]string{"remove", "id"}, "", "DELETE /environments/id"},
	} {
		command := &cobra.Command{Use: "env"}
		addEnvironmentDirectoryCommands(command)
		var out bytes.Buffer
		command.SetOut(&out)
		command.SetErr(&out)
		command.SetIn(strings.NewReader(tc.input))
		command.SetArgs(tc.args)
		if err := command.Execute(); err != nil {
			t.Fatal(err)
		}
		if calls[len(calls)-1] != tc.method {
			t.Fatal("daemon call", calls)
		}
		if tc.args[0] == "remove" && !strings.Contains(out.String(), `"revocationPending":true`) {
			t.Fatal("lost pending status")
		}
	}
	command := &cobra.Command{Use: "env"}
	addEnvironmentDirectoryCommands(command)
	command.SetArgs([]string{"register"})
	command.SetIn(strings.NewReader(`{"authority":"worker","grant":"admin"}`))
	command.SetOut(new(bytes.Buffer))
	command.SetErr(new(bytes.Buffer))
	before := len(calls)
	if err := command.Execute(); err == nil || len(calls) != before {
		t.Fatal("unknown field reached daemon")
	}
}
