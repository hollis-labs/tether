package main

import (
	"testing"
	"time"
)

func TestCatalogPlanTTL(t *testing.T) {
	for in, want := range map[string]time.Duration{"": 0, "junk": 0, "30s": 0, "169h": 0, "168h": 168 * time.Hour, "25h": 25 * time.Hour, "4h": 4 * time.Hour, " 24h ": 24 * time.Hour, "1m": time.Minute} {
		if got := catalogPlanTTL(in); got != want {
			t.Errorf("catalogPlanTTL(%q)=%v want %v", in, got, want)
		}
	}
}
