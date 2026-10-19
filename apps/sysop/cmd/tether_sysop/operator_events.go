package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/eventstamp"
	"github.com/hollis-labs/tether/internal/store"
)

var operatorMetadataID = regexp.MustCompile(`^[A-Za-z0-9._:/@-]{1,256}$`)

// Observations exclude checkpoint summaries/status prose and policy status
// prose. The authoritative action keeps its original content.
func operatorObservation(kind string, input map[string]any) (map[string]any, error) {
	actions := map[string]string{kindSysopSessionStop: "stop", kindSysopSessionCheckpoint: "checkpoint", kindSysopLogicalResume: "resume", kindSysopSessionCleanup: "cleanup", kindSysopLogicalAgentPolicy: "logical_agent_policy_update"}
	action, ok := actions[kind]
	if !ok {
		return nil, fmt.Errorf("unsupported operator observation kind")
	}
	out := map[string]any{"source": "sysop", "action": action}
	switch kind {
	case kindSysopSessionCheckpoint:
		if n, ok := input["summary_bytes"].(int); ok && n >= 0 {
			out["summary_bytes"] = n
		}
	case kindSysopSessionCleanup:
		for _, key := range []string{"older_than_days", "limit", "deleted_count"} {
			if n, ok := input[key].(int); ok && n >= 0 {
				out[key] = n
			}
		}
	case kindSysopLogicalResume, kindSysopLogicalAgentPolicy:
		for _, key := range []string{"logical_agent_id", "provider_id", "provider_kind"} {
			if kind == kindSysopLogicalAgentPolicy && key != "logical_agent_id" {
				continue
			}
			if v, ok := input[key].(string); ok && operatorMetadataID.MatchString(v) {
				out[key] = v
			}
		}
		if kind == kindSysopLogicalAgentPolicy {
			if v, ok := input["checkpoint_policy"].(string); ok && (v == "manual" || v == "on_stop") {
				out["checkpoint_policy"] = v
			}
		}
	}
	return out, nil
}

func (s *appServer) recordOperatorEvent(scope events.Scope, sessionID, kind string, payload map[string]any) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	db, err := s.openStateDB()
	if err != nil {
		if !errors.Is(err, errStateDBUnset) {
			log.Printf("sysop: operator observation unavailable")
		}
		return
	}
	defer func() { _ = db.Close() }()
	if err = enqueueOperatorEvent(ctx, db, scope, sessionID, kind, payload); err != nil {
		log.Printf("sysop: operator observation enqueue failed")
	}
}

func enqueueOperatorEvent(ctx context.Context, db *store.Store, scope events.Scope, sessionID, kind string, payload map[string]any) error {
	metadata, err := operatorObservation(kind, payload)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	stamp := eventstamp.New()
	host, _ := os.Hostname()
	process := eventstamp.Process{Host: host}
	progress, err := db.EventIdentityProgress(ctx)
	if err == nil {
		process.EnvironmentID = progress.EnvironmentID
	} else if !errors.Is(err, store.ErrEventIdentityUnseeded) {
		return err
	}
	stamp.SetProcess(process)
	bus := events.NewBus(events.BusOptions{Persister: db, Stages: []events.Stage{stamp}}).(events.OutboxBus)
	tx, err := db.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	e := events.Event{Scope: scope, SessionID: sessionID, Kind: kind, PayloadJSON: string(raw), Envelope: events.Envelope{SchemaVersion: runtimeevents.SchemaVersion, App: "tether-sysop", ContentType: "application/json", Class: events.Class{Category: "audit", Severity: "info", Sensitivity: "private", Visibility: "local", Sanitized: true}}}
	if sessionID != "" {
		d := &e.Envelope.Dims
		err = tx.QueryRowContext(ctx, `SELECT COALESCE(worker_id,''),COALESCE(project_id,''),COALESCE(workstream_id,''),COALESCE(task_id,''),COALESCE(launch_id,''),COALESCE(logical_agent_id,'') FROM sessions WHERE id=?`, sessionID).Scan(&d.WorkerID, &d.ProjectID, &d.WorkstreamID, &d.TaskID, &d.LaunchID, &d.LogicalAgentID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		e.LogicalAgentID = d.LogicalAgentID
	}
	// No verified operator principal exists here. Never manufacture one from
	// browser parameters, a display name or the fact that the action succeeded.
	if _, err = bus.PublishTx(ctx, tx, e); err != nil {
		return err
	}
	// The daemon sweeps pending rows. Local fan-out/clearing pending would hide
	// this event from its live subscribers.
	return tx.Commit()
}
