package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	tether "github.com/hollis-labs/substrate/mesh/tetherclient"
	directory "github.com/hollis-labs/tether/internal/environmentdirectory"
	"github.com/hollis-labs/tether/internal/store"
)

func directoryRecord(authority string) directory.Record {
	now := time.Now().UTC()
	return directory.Record{Registration: directory.Registration{EnvironmentTarget: tether.EnvironmentTarget{EnvironmentID: uuid.NewString(), Authority: authority, Routes: []tether.EnvironmentRoute{{BaseURL: "http://127.0.0.1:7777"}}, CredentialReference: "file:///synthetic-private-reference"}, DeviceID: "synthetic-device", Ownership: "external", Homes: []directory.Home{{URN: "msg://agent/" + authority + "/test-agent"}}}, Protocol: 1, ServerVersion: "synthetic", State: "reachable", LastSeen: &now, Capabilities: map[string]map[string]any{"lifecycle": {"enabled": true}}}
}

func TestEnvironmentDirectoryRestartIdentityAndRetirement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	one, two := directoryRecord("worker-one"), directoryRecord("worker-two")
	first, err := db.RegisterEnvironment(ctx, one)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.RegisterEnvironment(ctx, two); err != nil {
		t.Fatal(err)
	}
	rebound := one
	rebound.EnvironmentID = uuid.NewString()
	if _, err = db.RegisterEnvironment(ctx, rebound); !errors.Is(err, directory.ErrConflict) {
		t.Fatalf("duplicate authority err%v", err)
	}
	rebound = one
	rebound.Authority = "worker-other"
	rebound.Homes = nil
	if _, err = db.RegisterEnvironment(ctx, rebound); !errors.Is(err, directory.ErrConflict) {
		t.Fatalf("changed UUID binding err%v", err)
	}
	moved := two
	moved.Homes = one.Homes
	if _, err = db.RegisterEnvironment(ctx, moved); err == nil {
		t.Fatal("second home accepted")
	}
	renamed, err := db.RenameEnvironment(ctx, one.EnvironmentID, "new label")
	if err != nil || renamed.Authority != "worker-one" {
		t.Fatal("rename changed authority", err)
	}
	observed, err := db.ObserveEnvironment(ctx, one.EnvironmentID, "unreachable", nil, nil)
	if err != nil || !observed.LastSeen.Equal(*first.LastSeen) {
		t.Fatal("failure lost last successful seen", err)
	}
	retired, initial, err := db.RetireEnvironment(ctx, one.EnvironmentID)
	if err != nil || !initial || retired.State != "retired" || !retired.RevocationPending {
		t.Fatal("bad tombstone", err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	records, err := db.ListEnvironments(ctx)
	if err != nil || len(records) != 2 {
		t.Fatal("restart lost directory", err)
	}
	saved, err := db.GetEnvironment(ctx, one.EnvironmentID)
	if err != nil || saved.State != "retired" || saved.Label != "new label" || !saved.RevocationPending || saved.Capabilities["lifecycle"]["enabled"] != true {
		t.Fatalf("bad restart%+v err%v", saved, err)
	}
	if _, err = db.RegisterEnvironment(ctx, one); !errors.Is(err, directory.ErrRetired) {
		t.Fatal("retired identity recycled")
	}
	rebound.EnvironmentID = uuid.NewString()
	rebound.Authority = "worker-one"
	rebound.Homes = nil
	if _, err = db.RegisterEnvironment(ctx, rebound); !errors.Is(err, directory.ErrConflict) {
		t.Fatal("tombstone authority recycled", err)
	}
}

func TestEnvironmentDirectoryConcurrentAuthorityBinding(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, e := db.RegisterEnvironment(context.Background(), directoryRecord("worker-one"))
			results <- e
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	successes, conflicts := 0, 0
	for e := range results {
		if e == nil {
			successes++
		} else if errors.Is(e, directory.ErrConflict) {
			conflicts++
		} else {
			t.Fatal(e)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes%d conflicts%d", successes, conflicts)
	}
}
