package runtoken

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

const handoffPathEnv = "TETHER_RUN_HANDOFF_PATH"
const handoffIDEnv = "TETHER_RUN_HANDOFF_ID"

var ErrHandoff = errors.New("run token runtime handoff refused")

type handoffPayload struct {
	ID      string `json:"id"`
	Session string `json:"session"`
	Token   string `json:"token"`
}

// PrepareEnvironment removes the runtime token before any descriptor is written.
// Only a bound reference to nonpersistent memory enters the durable environment.
// The caller keeps release alive until the shim has read its descriptor and started.
func PrepareEnvironment(env []string, session string) ([]string, func(), error) {
	return prepareEnvironment(env, session, HandoffSupported())
}
func prepareEnvironment(env []string, session string, supported bool) ([]string, func(), error) {
	out := make([]string, 0, len(env)+2)
	token := ""
	for _, entry := range env {
		k, v, _ := strings.Cut(entry, "=")
		switch k {
		case TokenEnv:
			if token != "" && token != v {
				return nil, nil, ErrHandoff
			}
			token = v
		case handoffPathEnv, handoffIDEnv:
		default:
			out = append(out, entry)
		}
	}
	if token == "" {
		return out, func() {}, nil
	}
	if !supported || len(token) > MaxSize || session == "" {
		return nil, nil, ErrHandoff
	}
	payload := handoffPayload{ID: uuid.NewString(), Session: session, Token: token}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, ErrHandoff
	}
	path, release, err := memoryHandoff(data)
	if err != nil {
		return nil, nil, ErrHandoff
	}
	return append(out, handoffPathEnv+"="+path, handoffIDEnv+"="+payload.ID), release, nil
}

// RestoreEnvironment is called only by the runtime shim entrypoint, never by
// descriptor inspection or recovery. An unavailable/stale reference fails closed.
func RestoreEnvironment(env []string, session string) ([]string, error) {
	out := make([]string, 0, len(env))
	path, id := "", ""
	for _, entry := range env {
		k, v, _ := strings.Cut(entry, "=")
		switch k {
		case TokenEnv:
			return nil, ErrHandoff
		case handoffPathEnv:
			if path != "" {
				return nil, ErrHandoff
			}
			path = v
		case handoffIDEnv:
			if id != "" {
				return nil, ErrHandoff
			}
			id = v
		default:
			out = append(out, entry)
		}
	}
	if path == "" && id == "" {
		return out, nil
	}
	if path == "" || id == "" {
		return nil, ErrHandoff
	}
	data, err := readMemoryHandoff(path)
	if err != nil {
		return nil, ErrHandoff
	}
	var payload handoffPayload
	if json.Unmarshal(data, &payload) != nil || payload.ID != id || payload.Session != session || payload.Token == "" || len(payload.Token) > MaxSize {
		return nil, ErrHandoff
	}
	return append(out, TokenEnv+"="+payload.Token), nil
}

// SealEnvironment replaces reserved execution values from launch authority and
// removes ambient duplicates. Torque's TETHER_RUN_ID is owned by Correlation.
func SealEnvironment(env []string, authority map[string]string) []string {
	out := make([]string, 0, len(env)+2)
	for _, entry := range env {
		k, _, _ := strings.Cut(entry, "=")
		switch k {
		case TokenEnv, IDEnv, handoffPathEnv, handoffIDEnv:
		default:
			out = append(out, entry)
		}
	}
	for _, k := range []string{IDEnv, TokenEnv} {
		if v := authority[k]; v != "" {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// SealMap removes reserved execution claims before adding launcher-owned values.
func SealMap(env, authority map[string]string) map[string]string {
	out := make(map[string]string, len(env)+2)
	for k, v := range env {
		switch k {
		case TokenEnv, IDEnv, handoffPathEnv, handoffIDEnv:
		default:
			out[k] = v
		}
	}
	for _, k := range []string{IDEnv, TokenEnv} {
		if v := authority[k]; v != "" {
			out[k] = v
		}
	}
	return out
}

// FinishHandoff closes completed/unsubmitted handoffs immediately. A possibly
// submitted runtime owns an independent settlement lifetime: caller cancellation
// cannot revoke its first read. The proof must mean readiness or verified absence.
func FinishHandoff(env []string, release func(), uncertain bool, settled func(context.Context) bool) {
	hasReference := false
	for _, entry := range env {
		if strings.HasPrefix(entry, handoffPathEnv+"=") {
			hasReference = true
		}
	}
	if !uncertain || !hasReference {
		release()
		return
	}
	go func() {
		defer release()
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			done := settled(ctx)
			cancel()
			if done {
				return
			}
			<-ticker.C
		}
	}()
}
