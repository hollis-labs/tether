package app

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"time"

	"github.com/hollis-labs/substrate/harness/adapters/runtimeevents"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
)

// recordEnvironmentRequest runs only after observeRuntime's policy checks,
// before taking its observer mutex. Payload intentionally excludes params,
// answers, reasons, tool contents, or any credential/configuration material.
func (o *sessionTurnOutput) recordEnvironmentRequest(ev runtimeevents.Event) {
	if ev.Kind == runtimeevents.KindTurnCompleted || ev.Kind == runtimeevents.KindTurnFailed {
		o.recordEndedTurnRequests(ev)
		return
	}
	if ev.Kind != runtimeevents.KindAgentPermissionRequested && ev.Kind != runtimeevents.KindAgentPermissionResolved {
		return
	}
	if o.service.Bus == nil || ev.TurnID == "" || ev.Sequence == 0 {
		return
	}
	var data struct {
		RequestID json.RawMessage `json:"request_id"`
		Method    string          `json:"method"`
	}
	if json.Unmarshal(ev.Payload, &data) != nil {
		return
	}
	id := ""
	if len(data.RequestID) > 0 && string(data.RequestID) != "null" {
		id = "request:" + string(data.RequestID)
	}
	open := ev.Kind == runtimeevents.KindAgentPermissionRequested
	if id == "" {
		if open && ev.ID != "" {
			id = "event:" + ev.ID
		} else if !open && ev.ParentID != "" {
			id = "event:" + ev.ParentID
		}
	}
	if id == "" {
		return
	}
	kind := "approval"
	if strings.HasSuffix(strings.ToLower(data.Method), "requestuserinput") {
		kind = "question"
	}
	if !open {
		kind = ""
	} // the stored exact request decides its kind
	req := store.EnvironmentRequest{TurnID: ev.TurnID, RequestID: id, Kind: kind, Open: open, SourceSequence: ev.Sequence}
	raw, err := json.Marshal(req)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err = o.service.Bus.Publish(ctx, events.Event{Scope: events.ScopeSession, SessionID: o.row.ID, Kind: store.EnvironmentRequestKind, PayloadJSON: string(raw)}); err != nil {
		log.Printf("environment request status for session %s unavailable: %v", o.row.ID, err)
	}
}

// A real terminal turn event closes only requests from that exact turn. A
// detached/orphaned lifecycle event never calls this path.
func (o *sessionTurnOutput) recordEndedTurnRequests(ev runtimeevents.Event) {
	if o.service.Bus == nil || ev.TurnID == "" || ev.Sequence == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	base, err := o.service.Store.EnvironmentSnapshot(ctx, o.row.ID)
	if err != nil {
		return
	}
	for _, req := range base.Requests {
		if req.TurnID != ev.TurnID || !req.Open || req.SourceSequence > ev.Sequence {
			continue
		}
		req.Open = false
		req.Kind = ""
		req.SourceSequence = ev.Sequence
		raw, err := json.Marshal(req)
		if err != nil {
			return
		}
		if err = o.service.Bus.Publish(ctx, events.Event{Scope: events.ScopeSession, SessionID: o.row.ID, Kind: store.EnvironmentRequestKind, PayloadJSON: string(raw)}); err != nil {
			log.Printf("terminal request status for session %s unavailable: %v", o.row.ID, err)
		}
	}
}
