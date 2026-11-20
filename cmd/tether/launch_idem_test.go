package main

import "testing"

func TestDeriveLaunchIdempotencyKey(t *testing.T) {
	a := deriveLaunchIdempotencyKey("l", "w", []string{"T-2", "T-1"}, "")
	if b := deriveLaunchIdempotencyKey("l", "w", []string{"T-1", "T-2"}, ""); a != b {
		t.Fatalf("task order changed the key: %s vs %s", a, b)
	}
	if c := deriveLaunchIdempotencyKey("l", "w", []string{"T-1"}, ""); a == c {
		t.Fatal("different task sets share a key")
	}
	if d := deriveLaunchIdempotencyKey("l2", "w", []string{"T-1", "T-2"}, ""); a == d {
		t.Fatal("different launches share a key")
	}
}

func TestLaunchIdempotencyKeySeparatesTorqueRuns(t *testing.T) {
	if deriveLaunchIdempotencyKey("l", "w", []string{"task"}, "42") == deriveLaunchIdempotencyKey("l", "w", []string{"task"}, "43") {
		t.Fatal("different runs share auto launch key")
	}
}
