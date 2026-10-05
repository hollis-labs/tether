//go:build !windows

package app

import (
	"context"
	"errors"
	"testing"

	"github.com/hollis-labs/tether/internal/shimhost"
)

func TestShimRetirementRequiresCompleteControllerExclusion(t *testing.T) {
	s := &Service{}
	receipt := shimhost.Receipt{Session: "session"}
	if _, release, err := s.AcquireShimRetirementFence(context.Background(), receipt, "retire", nil); err == nil || release != nil {
		t.Fatal("missing exclusion accepted")
	}
	releases := 0
	exclude := func(context.Context) (func(), error) { return func() { releases++ }, errors.New("unknown exclusion") }
	if _, release, err := s.AcquireShimRetirementFence(context.Background(), receipt, "retire", exclude); err == nil || release != nil {
		t.Fatal("unknown exclusion accepted")
	}
	if releases != 1 || len(s.launches) != 0 {
		t.Fatalf("exclusion release=%d launch gates=%d", releases, len(s.launches))
	}
}
