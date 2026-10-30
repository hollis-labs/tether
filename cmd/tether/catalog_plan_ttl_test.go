package main

import (
	"testing"
	"time"
)

func TestCatalogPlanTTL(t *testing.T) {
	for in, want := range map[string]time.Duration{"": 0, "junk": 0, "30s": 0, "25h": 0, "4h": 4 * time.Hour, " 24h ": 24 * time.Hour, "1m": time.Minute} {
		if got := catalogPlanTTL(in); got != want {
			t.Errorf("catalogPlanTTL(%q)=%v want %v", in, got, want)
		}
	}
}
