package workerruntime

import (
	"context"
	"errors"
	"fmt"
	"github.com/hollis-labs/tether/internal/identity"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDeviceRenewalScheduleAndFailureIsolation(t *testing.T) {
	calls, failures, steps := 0, 0, 0
	connected := false
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waits := []time.Duration{deviceRenewInterval, deviceRenewRetry, deviceRenewRetry, deviceRenewInterval}
	renewalLoop(ctx, func(context.Context) error {
		calls++
		if calls == 2 {
			return errors.New("failure")
		}
		return nil
	}, func() bool { return connected }, func(_ context.Context, d time.Duration) bool {
		if d != waits[steps] {
			t.Errorf("step%d wait %s want %s", steps, d, waits[steps])
		}
		steps++
		// First scheduled run is disconnected and must not issue a request.
		if steps == 2 {
			connected = true
		}
		if steps == 4 {
			cancel()
			return false
		}
		return true
	}, func() { failures++ })
	if calls != 3 || failures != 1 {
		t.Fatalf("calls=%d failures=%d", calls, failures)
	}
}
func TestDeviceRenewReadsCurrentTokenFileAndStopsOnCancellation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "device.token")
	// Real format accepted by ReadTokenFile.
	token, err := identity.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	hits := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("wrong credential")
		}
		fmt.Fprintf(w, `{"expires_at":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	}))
	defer s.Close()
	worker := &Worker{Root: root, Config: Config{Hub: s.URL}}
	if err := worker.renewDevice(context.Background()); err != nil {
		t.Fatal(err)
	}
	token, err = identity.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	if err := worker.renewDevice(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	renewalLoop(ctx, func(context.Context) error { t.Fatal("renewed after cancel"); return nil }, func() bool { return true }, func(context.Context, time.Duration) bool { t.Fatal("waited after cancel"); return false }, func() {})
	if hits != 2 {
		t.Fatalf("requests=%d", hits)
	}
}
