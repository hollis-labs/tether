package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// WorkerNativeAdmission is a historical hub observation of successful native
// readiness, not a request or a claim inferred from lineage. It is deliberately
// absent from launch.Plan: typed plan copies must never inherit this receipt.
type WorkerNativeAdmission struct {
	Kind                  string `json:"kind"`
	SourceSessionID       string `json:"source_session_id"`
	SourceGeneration      string `json:"source_generation"`
	ReplacementSessionID  string `json:"replacement_session_id"`
	ReplacementGeneration string `json:"replacement_generation"`
	WorkerID              string `json:"worker_id"`
	LogicalAgentID        string `json:"logical_agent_id"`
	NativeThreadID        string `json:"native_thread_id"`
	Workroot              string `json:"workroot"`
	AdmittedAt            string `json:"admitted_at"`
}

// RecordWorkerNativeAdmission is called only after the hub validates the worker's
// native readiness response. The server-only member uses existing durable JSON;
// request decoding and typed plan persistence cannot supply or copy it.
func (s *Store) RecordWorkerNativeAdmission(receipt WorkerNativeAdmission) error {
	receipt.Kind = "codex_same_worker_v1"
	receipt.AdmittedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := s.validateWorkerNativeAdmission(receipt); err != nil {
		return err
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	result, err := s.db.Exec(`UPDATE launch_plans SET plan_json=json_set(plan_json,'$._worker_native_admission',json(?))
 WHERE session_id=? AND json_type(plan_json,'$._worker_native_admission') IS NULL`, string(raw), receipt.ReplacementSessionID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("native admission receipt already recorded or session missing")
	}
	return nil
}

// GetWorkerNativeAdmission returns nil for old, fresh and unconfirmed attempts.
// No mapping or launch intent is promoted into proof on read.
func (s *Store) GetWorkerNativeAdmission(id string) (*WorkerNativeAdmission, error) {
	var raw sql.NullString
	err := s.db.QueryRow(`SELECT json_extract(plan_json,'$._worker_native_admission') FROM launch_plans WHERE session_id=?`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) || err == nil && !raw.Valid {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var receipt WorkerNativeAdmission
	if err := json.Unmarshal([]byte(raw.String), &receipt); err != nil {
		return nil, err
	}
	if receipt.ReplacementSessionID != id {
		return nil, fmt.Errorf("native admission session mismatch")
	}
	if err := s.validateWorkerNativeAdmission(receipt); err != nil {
		return nil, err
	}
	return &receipt, nil
}

func (s *Store) validateWorkerNativeAdmission(r WorkerNativeAdmission) error {
	if r.Kind != "codex_same_worker_v1" || r.SourceSessionID == "" || r.SourceGeneration == "" || r.ReplacementSessionID == "" || r.ReplacementGeneration == "" || r.WorkerID == "" || r.LogicalAgentID == "" || r.NativeThreadID == "" || r.Workroot == "" || r.AdmittedAt == "" || r.SourceSessionID == r.ReplacementSessionID {
		return fmt.Errorf("incomplete native admission receipt")
	}
	row, err := s.GetSession(r.ReplacementSessionID)
	if err != nil {
		return err
	}
	source, err := s.GetSession(r.SourceSessionID)
	if err != nil {
		return err
	}
	plan, err := s.GetLaunchPlan(r.ReplacementSessionID)
	if err != nil {
		return err
	}
	if row.WorkerID != r.WorkerID || source.WorkerID != r.WorkerID || row.LogicalAgentID != r.LogicalAgentID || source.LogicalAgentID != r.LogicalAgentID || row.ParentSessionID.String != r.SourceSessionID || !plan.NativeResumeOnly || plan.ResumeSourceSessionID != r.SourceSessionID || plan.WorkerSourceGeneration != r.SourceGeneration || plan.ResumeProviderSessionID != r.NativeThreadID || plan.WorkRoot != r.Workroot {
		return fmt.Errorf("native admission receipt conflicts with saved session")
	}
	return nil
}
