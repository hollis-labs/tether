package seatstatus

import (
	"errors"
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	now := time.Date(2026, 11, 14, 4, 0, 0, 0, time.UTC)
	ok := []Input{
		{State: Working, Reason: "building 0013", Ref: "task:CW-20261114-0013"},
		{State: Blocked, Owner: "@archie", Reason: "waiting on libs tag", TTL: time.Hour},
		{State: Available},
		{State: Paused, ExpiresAt: now.Add(48 * time.Hour), Ref: "https://github.com/hollis-labs/tether/pull/539"},
	}
	for _, in := range ok {
		if _, err := in.Validate(now); err != nil {
			t.Errorf("%+v: %v", in, err)
		}
	}
	bad := map[string]Input{
		"state":      {State: "busy"},
		"owner":      {State: Blocked},
		"reason":     {State: Working, Reason: "a\nb"},
		"ref":        {State: Working, Ref: "CW-1"},
		"ttl":        {State: Working, TTL: time.Hour, ExpiresAt: now.Add(time.Hour)},
		"expires_at": {State: Working, TTL: 8 * 24 * time.Hour},
	}
	for field, in := range bad {
		_, err := in.Validate(now)
		var e *Error
		if !errors.As(err, &e) || e.Field != field {
			t.Errorf("%s: err = %v", field, err)
		}
	}
	exp, _ := Input{State: Working, TTL: time.Hour}.Validate(now)
	if !exp.Equal(now.Add(time.Hour)) {
		t.Fatalf("expiry = %v", exp)
	}
}

func TestSameAsAndExpired(t *testing.T) {
	now := time.Now()
	a := Declared{State: Working, Reason: "x", ExpiresAt: now.Add(-time.Second)}
	b := a
	b.Since = now
	if !a.SameAs(b) || !a.Expired(now) || (Declared{}).Expired(now) {
		t.Fatal("SameAs/Expired")
	}
	b.Reason = "y"
	if a.SameAs(b) {
		t.Fatal("different reason is the same")
	}
}
