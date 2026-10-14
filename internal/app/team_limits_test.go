package app

import (
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/launchprofile"
)

func TestCapTeamMaxDuration(t *testing.T) {
	short, long := "30s", "1h"
	for name, tc := range map[string]struct {
		base *launchprofile.LifecyclePolicy
		cap  time.Duration
		want time.Duration
	}{
		"none":           {nil, 0, 0},
		"set on empty":   {nil, time.Minute, time.Minute},
		"lowers catalog": {&launchprofile.LifecyclePolicy{MaxDuration: &long}, time.Minute, time.Minute},
		"keeps tighter":  {&launchprofile.LifecyclePolicy{MaxDuration: &short}, time.Minute, 30 * time.Second},
		"zero is no cap": {&launchprofile.LifecyclePolicy{MaxDuration: &long}, 0, time.Hour},
	} {
		plan := &launch.Plan{Lifecycle: tc.base}
		capTeamMaxDuration(plan, tc.cap)
		d, err := plan.Lifecycle.Durations()
		if err != nil || d.MaxDuration != tc.want {
			t.Errorf("%s: got %v %v want %v", name, d.MaxDuration, err, tc.want)
		}
	}
}
