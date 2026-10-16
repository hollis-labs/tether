package eventretention

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

type snapshot struct {
	Version int            `json:"version"`
	Rules   map[Class]Rule `json:"rules"`
	PIICap  Age            `json:"pii_cap"`
}

// EncodeSnapshot serializes all resolved policy values, never user YAML or
// defaults that might change in a later binary. The content-derived ID is
// independently verified by the persistence adapter on every restore.
func (c Config) EncodeSnapshot() ([]byte, error) {
	if c.id == "" {
		return nil, fmt.Errorf("retention: uninitialized config")
	}
	return json.Marshal(snapshot{Version: 1, Rules: c.rules, PIICap: c.piiCap})
}

// RestoreSnapshot validates persisted policy values without re-asking for
// acknowledgment: admission already validated the operator's configuration.
func RestoreSnapshot(data []byte) (Config, error) {
	var stored snapshot
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&stored); err != nil {
		return Config{}, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return Config{}, fmt.Errorf("retention: extra snapshot data")
	}
	if stored.Version != 1 || len(stored.Rules) != 4 {
		return Config{}, fmt.Errorf("retention: unsupported or incomplete snapshot")
	}
	var raw rawConfig
	raw.Retention.Classes = map[Class]rawRule{}
	raw.Retention.PIICap = &stored.PIICap
	raw.Retention.AcknowledgeRisk = true
	for class, rule := range stored.Rules {
		raw.Retention.Classes[class] = rawRule{Full: &rule.Full, Envelope: &rule.Envelope, Rollups: &rule.Rollups}
	}
	c, _, err := compile(raw)
	return c, err
}
