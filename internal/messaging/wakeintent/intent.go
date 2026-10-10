// Package wakeintent carries recipient-side wake preferences through the
// existing messaging metadata contract. Preferences confer no wake authority.
package wakeintent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hollis-labs/substrate/mesh/messaging"
)

const MetadataKey = "tether.wake_intent"
const MessageIDKey = "tether.message_id"
const OutcomeKey = "tether.wake_outcome"

type localNotifyKey struct{}

// LocalNotify leaves the existing local /notify wake path in charge. This
// process-local flag never crosses the federation HTTP hop.
func LocalNotify(ctx context.Context) context.Context {
	return context.WithValue(ctx, localNotifyKey{}, true)
}
func IsLocalNotify(ctx context.Context) bool { v, _ := ctx.Value(localNotifyKey{}).(bool); return v }

type Intent struct {
	WakeText string `json:"wake_text,omitempty"`
	Urgency  string `json:"urgency,omitempty"`
	NoWake   bool   `json:"no_wake,omitempty"`
}

func Read(env messaging.Envelope) (Intent, error) {
	var intent Intent
	if raw := env.Metadata[MetadataKey]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &intent); err != nil {
			return intent, fmt.Errorf("invalid wake intent: %w", err)
		}
	}
	if len(intent.WakeText) > 64*1024 {
		return intent, fmt.Errorf("wake text too large")
	}
	switch intent.Urgency {
	case "", "very-low", "low", "normal", "high":
	default:
		return intent, fmt.Errorf("invalid wake urgency")
	}
	return intent, nil
}

func Put(env *messaging.Envelope, intent Intent) {
	metadata := make(map[string]string, len(env.Metadata)+1)
	for k, v := range env.Metadata {
		metadata[k] = v
	}
	raw, _ := json.Marshal(intent)
	metadata[MetadataKey] = string(raw)
	env.Metadata = metadata
}
