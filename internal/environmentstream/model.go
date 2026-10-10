// Package environmentstream projects Tether's durable environment event log
// onto the mesh contract. It does not grant any remote access authority.
package environmentstream

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
)

// Event is the published mesh envelope plus the two environment coordinates,
// flattened into the SAME JSON object. Neither extension shadows a mesh field.
type Event struct {
	mesh.Event
	EnvironmentID string `json:"environment_id"`
	Seq           int64  `json:"seq"`
}

// Map preserves the original kind and payload, including unknown kinds. The
// daemon is the reporting actor, not an invented original tool/user principal.
// Generation 1 is this durable projection's generation, not a shim generation.
// SourceSequence/ Cursor describe this source (the environment event table),
// never provider sequence, channel publication sequence, or raw byte offset.
func Map(environmentID string, e events.Event) (Event, error) {
	if environmentID == "" || e.Seq < 1 || strings.ContainsAny(e.Kind, "\r\n") {
		return Event{}, fmt.Errorf("missing durable event identity")
	}
	payload := json.RawMessage(e.PayloadJSON)
	if len(payload) == 0 {
		payload = json.RawMessage(`null`)
	}
	if !json.Valid(payload) {
		return Event{}, fmt.Errorf("event %d contains invalid JSON", e.Seq)
	}
	subject := mesh.URN("urn:tether:environment:" + environmentID)
	if e.SessionID != "" {
		subject = mesh.URN("urn:session:" + e.SessionID)
	}
	process, err := json.Marshal(struct {
		Scope string `json:"scope"`
	}{e.Scope})
	if err != nil {
		return Event{}, err
	}
	out := Event{EnvironmentID: environmentID, Seq: e.Seq, Event: mesh.Event{
		SchemaVersion: "1", ID: environmentID + ":" + strconv.FormatInt(e.Seq, 10), Kind: e.Kind, Time: e.At, App: "tether",
		SessionID: e.SessionID, Subject: subject, Process: process,
		Source: mesh.EventSource{Channel: "tether.events", Confidence: 1}, Actor: mesh.Actor{URN: mesh.URN("msg://service/tether/" + environmentID), Kind: mesh.ActorService},
		Generation: 1, SourceSequence: uint64(e.Seq), Cursor: strconv.FormatInt(e.Seq, 10), ContentType: "application/json", Visibility: "local", Payload: payload,
	}}
	return out, out.Validate()
}

type Session struct {
	ID              string              `json:"id"`
	InstanceID      string              `json:"instance_id,omitempty"`
	LogicalAgentID  string              `json:"logical_agent_id,omitempty"`
	Kind            string              `json:"kind"`
	Route           json.RawMessage     `json:"route,omitempty"`
	SessionState    mesh.SessionState   `json:"session_state"`
	InstanceStatus  mesh.InstanceStatus `json:"instance_status"`
	InstanceDetail  mesh.InstanceDetail `json:"instance_detail"`
	LastActivity    time.Time           `json:"last_activity"`
	PendingQuestion bool                `json:"pending_question"`
	PendingApproval bool                `json:"pending_approval"`
	// Existing durable events do not record interactive request lifetimes.
	// Unknown remains explicit rather than pretending false means observed clear.
	PendingKnown  bool `json:"pending_known"`
	QuestionKnown bool `json:"pending_question_known"`
	ApprovalKnown bool `json:"pending_approval_known"`
	requests      map[string]store.EnvironmentRequest
}

type Snapshot struct {
	EnvironmentID     string    `json:"environment_id"`
	HighWaterSeq      int64     `json:"high_water_seq"`
	EarliestAvailable int64     `json:"earliest_available"`
	Sessions          []Session `json:"sessions"`
}

func snapshot(environmentID string, base store.EnvironmentSnapshot) Snapshot {
	out := Snapshot{EnvironmentID: environmentID, HighWaterSeq: base.HighWater, EarliestAvailable: base.EarliestAvailable, Sessions: make([]Session, 0, len(base.Sessions))}
	for _, row := range base.Sessions {
		s := Session{ID: row.ID, LogicalAgentID: row.LogicalAgentID, Kind: row.ProviderKind}
		s.LastActivity, _ = time.Parse(time.RFC3339Nano, row.UpdatedAt)
		if activity, err := time.Parse(time.RFC3339Nano, row.LastEventAt); err == nil && activity.After(s.LastActivity) {
			s.LastActivity = activity
		}
		if json.Valid([]byte(row.RouteJSON)) {
			s.Route = json.RawMessage(row.RouteJSON)
		}
		setState(&s, row.State)
		if s.SessionState != mesh.SessionEnded {
			s.requests = make(map[string]store.EnvironmentRequest)
			for _, req := range base.Requests {
				if req.SessionID == s.ID {
					s.requests[req.TurnID+"\x00"+req.RequestID] = req
				}
			}
			if len(s.requests) > 0 {
				refreshRequests(&s)
			}
		}
		out.Sessions = append(out.Sessions, s)
	}
	return out
}
