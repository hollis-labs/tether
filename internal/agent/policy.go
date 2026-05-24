package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type CheckpointPolicy string

const (
	CheckpointPolicyManual CheckpointPolicy = "manual"
	CheckpointPolicyOnStop CheckpointPolicy = "on_stop"
)

const policyKeyCheckpointStatus = "checkpoint_status"

type LogicalAgentPolicy struct {
	LogicalAgentID   string
	Name             string
	LaunchID         string
	CheckpointPolicy CheckpointPolicy
	CheckpointStatus string
	UpdatedAt        string
}

func NormalizeCheckpointPolicy(raw string) (CheckpointPolicy, error) {
	switch CheckpointPolicy(strings.TrimSpace(raw)) {
	case "", CheckpointPolicyManual:
		return CheckpointPolicyManual, nil
	case CheckpointPolicyOnStop:
		return CheckpointPolicyOnStop, nil
	default:
		return "", fmt.Errorf("unsupported checkpoint_policy %q", raw)
	}
}

func (p *LogicalAgentPolicy) Normalize() error {
	if strings.TrimSpace(p.LogicalAgentID) == "" {
		return errors.New("logical_agent_id required")
	}
	mode, err := NormalizeCheckpointPolicy(string(p.CheckpointPolicy))
	if err != nil {
		return err
	}
	p.LogicalAgentID = strings.TrimSpace(p.LogicalAgentID)
	p.Name = strings.TrimSpace(p.Name)
	p.LaunchID = strings.TrimSpace(p.LaunchID)
	p.CheckpointPolicy = mode
	p.CheckpointStatus = strings.TrimSpace(p.CheckpointStatus)
	if p.CheckpointPolicy != CheckpointPolicyOnStop {
		p.CheckpointStatus = ""
	}
	return nil
}

func DecodeCheckpointStatus(policiesJSON string) (string, error) {
	policiesJSON = strings.TrimSpace(policiesJSON)
	if policiesJSON == "" {
		return "", nil
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(policiesJSON), &payload); err != nil {
		return "", fmt.Errorf("decode policies_json: %w", err)
	}
	var status string
	if raw, ok := payload[policyKeyCheckpointStatus]; ok && len(raw) > 0 {
		if err := json.Unmarshal(raw, &status); err != nil {
			return "", fmt.Errorf("decode checkpoint_status: %w", err)
		}
	}
	return strings.TrimSpace(status), nil
}

func MergeCheckpointStatus(policiesJSON, status string) (string, error) {
	policiesJSON = strings.TrimSpace(policiesJSON)
	status = strings.TrimSpace(status)

	payload := map[string]json.RawMessage{}
	if policiesJSON != "" {
		if err := json.Unmarshal([]byte(policiesJSON), &payload); err != nil {
			return "", fmt.Errorf("decode policies_json: %w", err)
		}
	}
	if status == "" {
		delete(payload, policyKeyCheckpointStatus)
	} else {
		b, err := json.Marshal(status)
		if err != nil {
			return "", fmt.Errorf("encode checkpoint_status: %w", err)
		}
		payload[policyKeyCheckpointStatus] = b
	}
	if len(payload) == 0 {
		return "", nil
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode policies_json: %w", err)
	}
	return string(b), nil
}
