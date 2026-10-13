package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchTorqueTaskSendsBearerAndHintsOn401(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		if got != "Bearer tok" {
			http.Error(w, "nope", http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"id":"T-1"}`))
	}))
	defer srv.Close()

	t.Setenv("TORQUE_API_TOKEN", "")
	_, err := fetchTorqueTask(context.Background(), srv.Client(), srv.URL, "T-1")
	if err == nil || !strings.Contains(err.Error(), "TORQUE_API_TOKEN") {
		t.Fatalf("want 401 hint naming TORQUE_API_TOKEN, got %v", err)
	}
	t.Setenv("TORQUE_API_TOKEN", "tok")
	if _, err := fetchTorqueTask(context.Background(), srv.Client(), srv.URL, "T-1"); err != nil {
		t.Fatal(err)
	}
}
