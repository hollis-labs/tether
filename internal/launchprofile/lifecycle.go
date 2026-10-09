package launchprofile

import (
	"fmt"
	"time"
)

// LifecyclePolicy is a field-wise cascade: nil inherits, "0s" explicitly
// disables a limit. No wall-clock ceiling is enabled implicitly.
type LifecyclePolicy struct {
	IdleTimeout    *string `yaml:"idle_timeout,omitempty" json:"idle_timeout,omitempty"`
	MaxDuration    *string `yaml:"max_duration,omitempty" json:"max_duration,omitempty"`
	LeaseDuration  *string `yaml:"lease_duration,omitempty" json:"lease_duration,omitempty"`
	OrphanGrace    *string `yaml:"orphan_grace,omitempty" json:"orphan_grace,omitempty"`
	RequestGrace   *string `yaml:"request_grace,omitempty" json:"request_grace,omitempty"`
	TerminateGrace *string `yaml:"terminate_grace,omitempty" json:"terminate_grace,omitempty"`
	KillGrace      *string `yaml:"kill_grace,omitempty" json:"kill_grace,omitempty"`
}

type LifecycleDurations struct {
	IdleTimeout, MaxDuration, LeaseDuration              time.Duration
	OrphanGrace, RequestGrace, TerminateGrace, KillGrace time.Duration
}

func MergeLifecycle(base, override *LifecyclePolicy) *LifecyclePolicy {
	if base == nil && override == nil {
		return nil
	}
	out := &LifecyclePolicy{}
	for _, p := range []*LifecyclePolicy{base, override} {
		if p == nil {
			continue
		}
		for dst, src := range map[**string]*string{&out.IdleTimeout: p.IdleTimeout, &out.MaxDuration: p.MaxDuration, &out.LeaseDuration: p.LeaseDuration, &out.OrphanGrace: p.OrphanGrace, &out.RequestGrace: p.RequestGrace, &out.TerminateGrace: p.TerminateGrace, &out.KillGrace: p.KillGrace} {
			if src != nil {
				v := *src
				*dst = &v
			}
		}
	}
	return out
}

func (p *LifecyclePolicy) Durations() (LifecycleDurations, error) {
	d := LifecycleDurations{OrphanGrace: 30 * time.Second, RequestGrace: 2 * time.Second, TerminateGrace: 5 * time.Second, KillGrace: 5 * time.Second}
	if p == nil {
		return d, nil
	}
	fields := []struct {
		name   string
		value  *string
		target *time.Duration
	}{
		{"idle_timeout", p.IdleTimeout, &d.IdleTimeout}, {"max_duration", p.MaxDuration, &d.MaxDuration}, {"lease_duration", p.LeaseDuration, &d.LeaseDuration},
		{"orphan_grace", p.OrphanGrace, &d.OrphanGrace}, {"request_grace", p.RequestGrace, &d.RequestGrace}, {"terminate_grace", p.TerminateGrace, &d.TerminateGrace}, {"kill_grace", p.KillGrace, &d.KillGrace},
	}
	for _, f := range fields {
		if f.value == nil {
			continue
		}
		v, err := time.ParseDuration(*f.value)
		if err != nil || v < 0 {
			return d, fmt.Errorf("lifecycle.%s must be a non-negative duration", f.name)
		}
		*f.target = v
	}
	return d, nil
}
