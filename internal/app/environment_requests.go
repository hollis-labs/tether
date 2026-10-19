package app

import (
	"context"
	"encoding/json"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
)

// recordEnvironmentRequest runs only after observeRuntime's policy checks,
// before taking its observer mutex. Payload intentionally excludes params,
// answers, reasons, tool contents, or any credential/configuration material.
func (o *sessionTurnOutput) recordEnvironmentRequest(ev runtimeevents.Event, origin *outputEventOrigin) {
	if err := o.service.recordRuntimeRequest(o.row, ev, origin); err != nil {
		log.Printf("environment request status for session %s unavailable: %v", o.row.ID, err)
	}
}

func (s *Service) recordRuntimeRequest(row store.SessionRow, ev runtimeevents.Event, origin *outputEventOrigin) error {
	if ev.Kind == runtimeevents.KindTurnCompleted || ev.Kind == runtimeevents.KindTurnFailed {
		return s.recordEndedTurnRequests(row, ev, origin)
	}
	if ev.Kind != runtimeevents.KindAgentPermissionRequested && ev.Kind != runtimeevents.KindAgentPermissionResolved {
		return nil
	}
	if s.Bus == nil || ev.TurnID == "" || ev.Sequence == 0 {
		return nil
	}
	var data struct {
		RequestID json.RawMessage `json:"request_id"`
		Method    string          `json:"method"`
	}
	if json.Unmarshal(ev.Payload, &data) != nil {
		return nil
	}
	id := scalarRequestID(data.RequestID)
	open := ev.Kind == runtimeevents.KindAgentPermissionRequested
	if id == "" {
		if open && ev.ID != "" {
			id = "event:" + ev.ID
		} else if !open && ev.ParentID != "" {
			id = "event:" + ev.ParentID
		}
	}
	if id == "" {
		return nil
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
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err = s.Bus.Publish(ctx, origin.apply(events.Event{Scope: events.ScopeSession, SessionID: row.ID, LogicalAgentID: row.LogicalAgentID, Kind: store.EnvironmentRequestKind, PayloadJSON: string(raw)})); err != nil {
		return err
	}
	return nil
}

// JSON-RPC identities are scalars. Never turn an arbitrary object/array or
// request params into supposedly nonsecret request identity metadata.
func scalarRequestID(raw json.RawMessage) string {
	if len(raw) == 0 || len(raw) > 512 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if text == "" {
			return ""
		}
		canonical, _ := json.Marshal(text)
		return "request:" + string(canonical)
	}
	number := strings.TrimSpace(string(raw))
	if _, err := strconv.ParseInt(number, 10, 64); err == nil {
		return "request:" + number
	}
	return ""
}

// A real terminal turn event closes only requests from that exact turn. A
// detached/orphaned lifecycle event never calls this path.
func (s *Service) recordEndedTurnRequests(row store.SessionRow, ev runtimeevents.Event, origin *outputEventOrigin) error {
	if s.Bus == nil || ev.TurnID == "" || ev.Sequence == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := store.EnvironmentRequest{TurnID: ev.TurnID, Terminal: true, SourceSequence: ev.Sequence}
	raw, err := json.Marshal(req)
	if err != nil {
		return err
	}
	return s.Bus.Publish(ctx, origin.apply(events.Event{Scope: events.ScopeSession, SessionID: row.ID, LogicalAgentID: row.LogicalAgentID, Kind: store.EnvironmentRequestKind, PayloadJSON: string(raw)}))
}
