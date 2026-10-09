package app

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/hollis-labs/substrate/harness/adapters/turnoutput"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"github.com/hollis-labs/tether/internal/store"
)

// Only output and its routing attribution enter the journal; a launch plan can
// contain credentials and must never be copied here. Unrouted bodies retain the
// same bounded excerpt as the bus, rather than a new full-text archive.
type outputRetryRecord struct {
	Version           int                  `json:"version"`
	SessionID         string               `json:"session_id"`
	LogicalAgentID    string               `json:"logical_agent_id"`
	ProjectID         string               `json:"project_id"`
	ProviderResultID  string               `json:"provider_result_id,omitempty"`
	Route             *launchprofile.Route `json:"route,omitempty"`
	RouteUnread       bool                 `json:"route_unread,omitempty"`
	Result            turnoutput.Output    `json:"result"`
	TextTruncated     bool                 `json:"text_truncated,omitempty"`
	FreshConversation bool                 `json:"fresh_conversation,omitempty"`
}

func (w *turnOutputWrite) journal(s *Service) error {
	if w.journaled {
		return nil
	}
	record := outputRetryRecord{Version: 1, SessionID: w.row.ID, LogicalAgentID: w.row.LogicalAgentID,
		ProjectID: w.row.ProjectID, ProviderResultID: w.providerResultID, Route: w.route, RouteUnread: w.routeUnread,
		Result: w.result, TextTruncated: w.textTruncated, FreshConversation: w.freshConversation}
	if !w.routeUnread && (w.route == nil || !slices.Contains(w.route.Kinds, string(w.result.Kind))) {
		record.Result.Text, record.TextTruncated = turnOutputExcerpt(w.result.Text)
		record.TextTruncated = record.TextTruncated || w.textTruncated
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	// Native source identity and body stay stable across route/attribution or
	// continuity changes during replay. Those annotations cannot mint output.
	id := store.TurnOutputID(w.row.ID, w.result.TurnID, string(w.result.Kind), w.providerResultID, w.result.Text)
	w.journalID = id
	if err := s.Store.WriteTurnOutputRetry(id, data); err != nil {
		return err
	}
	w.journaled = true
	return nil
}

func readOutputRetry(s *Service, id string) (*turnOutputWrite, error) {
	data, err := s.Store.ReadTurnOutputRetry(id)
	if err != nil {
		return nil, err
	}
	var record outputRetryRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, fmt.Errorf("invalid output retry %q", id)
	}
	if record.Version != 1 || record.SessionID == "" {
		return nil, fmt.Errorf("unsupported output retry %q", id)
	}
	return &turnOutputWrite{row: store.SessionRow{ID: record.SessionID, LogicalAgentID: record.LogicalAgentID,
		ProjectID: record.ProjectID}, providerResultID: record.ProviderResultID, route: record.Route,
		routeUnread: record.RouteUnread, result: record.Result, textTruncated: record.TextTruncated,
		freshConversation: record.FreshConversation, journalID: id, journaled: true}, nil
}
