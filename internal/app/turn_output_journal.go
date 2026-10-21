package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/hollis-labs/libs/harness/core/adapters/turnoutput"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"github.com/hollis-labs/tether/internal/store"
)

// Only output and its routing attribution enter the journal; a launch plan can
// contain credentials and must never be copied here. Unrouted bodies retain the
// same bounded excerpt as the bus, rather than a new full-text archive.
type outputRetryRecord struct {
	WithheldReason    string               `json:"withheld_reason,omitempty"`
	WithheldSize      int                  `json:"withheld_size,omitempty"`
	Origin            *outputEventOrigin   `json:"origin,omitempty"`
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
	// Audience is who asked for the turn; a replay republishes it unchanged.
	Audience *store.TurnAudience `json:"audience,omitempty"`
}

func (w *turnOutputWrite) journal(s *Service) error {
	if w.invariantFailed {
		return store.ErrDuplicateWithheldOutput
	}
	if w.journalID == "" {
		if w.withheldReason == "secret_withheld" || (w.origin != nil && w.origin.Envelope.Class.Sensitivity == "secret") {
			w.journalID = store.WithheldTurnOutputID(w.row.ID, w.result.TurnID, string(w.result.Kind), w.providerResultID)
		} else {
			w.journalID = store.TurnOutputID(w.row.ID, w.result.TurnID, string(w.result.Kind), w.providerResultID, w.result.Text)
		}
	}
	w.prepareOutputPersistence(s)
	if w.journaled {
		return nil
	}
	if w.journalClaimed {
		if err := s.Store.SyncTurnOutputRetry(w.journalID); err != nil {
			return err
		}
		w.journaled = true
		return nil
	}
	record := outputRetryRecord{Version: 1, WithheldReason: w.withheldReason, WithheldSize: w.withheldSize, Origin: w.origin, SessionID: w.row.ID, LogicalAgentID: w.row.LogicalAgentID,
		ProjectID: w.row.ProjectID, ProviderResultID: w.providerResultID, Route: w.route, RouteUnread: w.routeUnread,
		Result: w.result, TextTruncated: w.textTruncated, FreshConversation: w.freshConversation, Audience: w.audience}
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
	id := w.journalID
	if w.rewriteJournalFrom != "" {
		s.withheldOutputMu.Lock()
		defer s.withheldOutputMu.Unlock()
		if err := s.Store.RewriteTurnOutputRetry(w.rewriteJournalFrom, id, data); err != nil {
			return w.recordOutputInvariant(s, err)
		}
	} else if w.withheldReason == "secret_withheld" {
		// Serialize new producer claims across the journal/published boundary.
		// Recovered jobs already have journaled=true and never claim again.
		s.withheldOutputMu.Lock()
		defer s.withheldOutputMu.Unlock()
		ctx, cancel := s.outputPersistenceContext()
		found, err := s.Store.HasPublishedTurnOutput(ctx, w.row.ID, id)
		cancel()
		if err != nil {
			return err
		}
		if found {
			return w.recordOutputInvariant(s, store.ErrDuplicateWithheldOutput)
		}
		if err := s.Store.WriteWithheldTurnOutputRetry(id, data); err != nil {
			w.journalClaimed = errors.Is(err, store.ErrOutputRetryClaimed)
			return w.recordOutputInvariant(s, err)
		}
	} else if err := s.Store.WriteTurnOutputRetry(id, data); err != nil {
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
	job := &turnOutputWrite{withheldReason: record.WithheldReason, withheldSize: record.WithheldSize, origin: record.Origin, row: store.SessionRow{ID: record.SessionID, LogicalAgentID: record.LogicalAgentID,
		ProjectID: record.ProjectID}, providerResultID: record.ProviderResultID, route: record.Route,
		routeUnread: record.RouteUnread, result: record.Result, textTruncated: record.TextTruncated,
		freshConversation: record.FreshConversation, audience: record.Audience, journalID: id, journaled: true}
	withheldID := store.WithheldTurnOutputID(job.row.ID, job.result.TurnID, string(job.result.Kind), job.providerResultID)
	if (job.origin != nil && job.origin.Envelope.Class.Sensitivity == "secret") || (job.withheldReason == "secret_withheld" && id != withheldID) {
		// Scrub the body and replace its old content-derived name before this
		// job can enter a retry worker. A safe notice under the old name is an
		// interrupted upgrade: finish the rename without counting it again.
		job.journaled = false
		job.rewriteJournalFrom = id
		job.journalID = withheldID
		if err := job.journal(s); err != nil {
			return nil, fmt.Errorf("rewrite secret output retry %q: %w", id, err)
		}
	}
	return job, nil
}

func (w *turnOutputWrite) recordOutputInvariant(s *Service, err error) error {
	if errors.Is(err, store.ErrDuplicateWithheldOutput) && !w.invariantFailed {
		w.invariantFailed = true
		s.secretOutputDuplicate.Add(1)
	}
	return err
}
