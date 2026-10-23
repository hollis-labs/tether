package store

import (
	"path/filepath"
	"testing"
)

// CW-20261023-0018: synchronous is a config option; unset keeps SQLite's FULL.
func TestOpenSynchronousFromEnv(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want int
	}{{"", 2}, {"FULL", 2}, {"normal", 1}, {" EXTRA ", 3}} {
		t.Run("env="+tc.env, func(t *testing.T) {
			t.Setenv(SynchronousEnv, tc.env)
			db, err := Open(filepath.Join(t.TempDir(), "sync.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var got int
			if err := db.DB().QueryRow(`PRAGMA synchronous`).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("PRAGMA synchronous = %d with %s=%q, want %d", got, SynchronousEnv, tc.env, tc.want)
			}
		})
	}
	t.Run("invalid value is refused", func(t *testing.T) {
		t.Setenv(SynchronousEnv, "OFF")
		if db, err := Open(filepath.Join(t.TempDir(), "sync.db")); err == nil {
			db.Close()
			t.Fatal("Open accepted synchronous=OFF")
		}
	})
}
