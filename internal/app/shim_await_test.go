package app

import (
	"testing"
	"time"
)

const shimFixtureBudget = 8 * time.Second

func shimAwait(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(shimFixtureBudget)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timeout: " + what)
}
