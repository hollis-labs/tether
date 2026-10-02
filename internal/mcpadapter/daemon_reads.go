package mcpadapter

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/hollis-labs/tether/internal/app/proxyevents"
	"log/slog"
	"time"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/checkpoint"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
)

// The observation tools' reads, in-process or over the daemon API (see
// readsViaDaemon). The daemon's DTOs are mapped back onto the store's types,
// so a tool's result has the same shape whichever way it was read.

func (a *Adapter) sessionEvents(ctx context.Context, sessionID string, limit int, cursor int64) ([]events.Event, error) {
	if !a.readsViaDaemon() {
		evs, err := a.svc.Store.ListEventsBySession(sessionID, limit, cursor)
		if err != nil {
			return nil, toolError("internal_error", "list events: "+err.Error())
		}
		return evs, nil
	}
	res, err := a.client.SessionEvents(ctx, sessionID, limit, cursor)
	if err != nil {
		return nil, daemonReadError(err, sessionID)
	}
	out := make([]events.Event, 0, len(res.Events))
	for _, e := range res.Events {
		at, _ := time.Parse(time.RFC3339Nano, e.At)
		out = append(out, events.Event{Seq: e.Seq, At: at, Scope: e.Scope, SessionID: e.SessionID, Kind: e.Kind, PayloadJSON: e.PayloadJSON})
	}
	return out, nil
}

// sessionCheckpoints lists the checkpoints of sessionID's logical agent. Read
// over the daemon, a checkpoint carries no ProviderHintsJSON, which
// GET /sessions/{id}/checkpoints does not return.
func (a *Adapter) sessionCheckpoints(ctx context.Context, sessionID string) ([]checkpoint.Checkpoint, error) {
	if !a.readsViaDaemon() {
		row, err := a.svc.Store.GetSession(sessionID)
		if err != nil {
			if isNotFound(err) {
				return nil, toolError("not_found", "session not found: "+sessionID)
			}
			return nil, toolError("internal_error", err.Error())
		}
		cps, err := a.svc.Store.ListCheckpointsByLogicalAgent(row.LogicalAgentID)
		if err != nil {
			return nil, toolError("internal_error", "list checkpoints: "+err.Error())
		}
		return cps, nil
	}
	dtos, err := a.client.SessionCheckpoints(ctx, sessionID)
	if err != nil {
		return nil, daemonReadError(err, sessionID)
	}
	out := make([]checkpoint.Checkpoint, 0, len(dtos))
	for _, c := range dtos {
		out = append(out, checkpoint.Checkpoint{
			ID:                  c.ID,
			LogicalAgentID:      c.LogicalAgentID,
			TaskID:              c.TaskID,
			WorkflowID:          c.WorkflowID,
			Status:              c.Status,
			CompletedWork:       c.CompletedWork,
			PendingWork:         c.PendingWork,
			KeyDecisions:        c.KeyDecisions,
			ReferencedArtifacts: c.ReferencedArtifacts,
			Summary:             c.Summary,
			NextRecommendation:  c.NextRecommendation,
			CreatedAt:           c.CreatedAt,
			SourceSessionID:     c.SourceSessionID,
		})
	}
	return out, nil
}

func (a *Adapter) sessionAttachments(ctx context.Context, sessionID string) ([]store.ClientAttachmentRow, error) {
	if !a.readsViaDaemon() {
		rows, err := a.svc.Store.ListClientAttachments(sessionID)
		if err != nil {
			return nil, toolError("internal_error", "list attachments: "+err.Error())
		}
		return rows, nil
	}
	dtos, err := a.client.SessionAttachments(ctx, sessionID)
	if err != nil {
		return nil, daemonReadError(err, sessionID)
	}
	out := make([]store.ClientAttachmentRow, 0, len(dtos))
	for _, d := range dtos {
		out = append(out, store.ClientAttachmentRow{
			ID:         d.ID,
			SessionID:  d.SessionID,
			ClientKind: d.ClientKind,
			AttachedAt: d.AttachedAt,
			DetachedAt: sql.NullString{String: d.DetachedAt, Valid: d.DetachedAt != ""},
		})
	}
	return out, nil
}

// proxyEventQuerier is where tether_proxy_events reads the proxy_events table.
func (a *Adapter) proxyEventQuerier() ProxyEventQuerier {
	if a.readsViaDaemon() {
		return DaemonProxyEvents{Client: a.client}
	}
	return a.svc.Store
}

// sessionWorkstreamID resolves legacy, unverified schema-1 correlation only.
func (a *Adapter) sessionWorkstreamID(ctx context.Context, sessionID string) (string, error) {
	if a.readsViaDaemon() {
		dto, err := a.client.GetSession(ctx, sessionID)
		if err != nil {
			return "", err
		}
		return dto.WorkstreamID, nil
	}
	row, err := a.svc.Store.GetSession(sessionID)
	if err != nil {
		return "", err
	}
	return row.WorkstreamID.String, nil
}

// DaemonProxyEvents is a ProxyEventQuerier over GET /proxy/events, for a
// daemon-only `tether mcp` (tether_events_tool_calls, tether_proxy_events).
type DaemonProxyEvents struct {
	Client *client.Client
}

// daemonQueryTimeout bounds a DaemonProxyEvents query, which ProxyEventQuerier
// gives no context.
const daemonQueryTimeout = 10 * time.Second

// QueryProxyEvents implements ProxyEventQuerier.
func (d DaemonProxyEvents) QueryProxyEvents(f store.ProxyEventFilter) ([]store.ProxyEvent, error) {
	ctx, cancel := context.WithTimeout(context.Background(), daemonQueryTimeout)
	defer cancel()
	dtos, err := d.Client.ProxyEvents(ctx, client.ProxyEventsQuery{
		SessionID:  f.SessionID,
		Server:     f.ServerID,
		ToolName:   f.ToolName,
		Limit:      f.Limit,
		ErrorsOnly: f.ErrorsOnly,
		Since:      f.Since,
	})
	if err != nil {
		return nil, err
	}
	out := make([]store.ProxyEvent, 0, len(dtos))
	for _, e := range dtos {
		ts, _ := time.Parse(time.RFC3339Nano, e.Timestamp)
		out = append(out, store.ProxyEvent{
			ToolCallDetails: e.ToolCallDetails,
			Attribution:     e.Attribution, ClaimedSessionID: e.ClaimedSessionID,
			ID: e.ID, SessionID: e.SessionID, Server: e.Server, ToolName: e.ToolName,
			ArgsSchemaFP: e.ArgsSchemaFP, DurationMs: e.DurationMs, OK: e.OK, Error: e.Error, Timestamp: ts,
		})
	}
	return out, nil
}

// DaemonToolCallPublisher is the events.Publisher a daemon-only `tether mcp`
// gives LoggingMiddleware: each tool_call_start and tool_call_end goes to the
// daemon's POST /proxy/events with publish set, so the daemon records the
// call in proxy_events and its event log, which this process cannot write.
//
// Publish never blocks a tool call on the daemon: events queue in order and
// one goroutine posts them. When the queue is full an event is dropped and
// logged, as the in-process bus drops for a slow subscriber.
type DaemonToolCallPublisher struct {
	client *client.Client
	queue  chan api.ProxyEventIngestRequest
}

const daemonToolCallQueue = 256

// NewDaemonToolCallPublisher starts a publisher posting to c until ctx ends.
func NewDaemonToolCallPublisher(ctx context.Context, c *client.Client) *DaemonToolCallPublisher {
	p := &DaemonToolCallPublisher{client: c, queue: make(chan api.ProxyEventIngestRequest, daemonToolCallQueue)}
	go p.run(ctx)
	return p
}

// Publish implements events.Publisher for tool call events; other kinds are
// ignored.
func (p *DaemonToolCallPublisher) Publish(_ context.Context, e events.Event) error {
	var phase string
	switch e.Kind {
	case events.EventTypeToolCallStart:
		phase = api.ProxyEventPhaseStart
	case events.EventTypeToolCallEnd:
		phase = api.ProxyEventPhaseEnd
	default:
		return nil
	}
	var tce events.ToolCallEvent
	if err := json.Unmarshal([]byte(e.PayloadJSON), &tce); err != nil {
		return err
	}
	req := api.ProxyEventIngestRequest(proxyevents.IngestCall(tce, phase, true))
	select {
	case p.queue <- req:
	default:
		slog.Warn("mcp: daemon tool call queue full; dropping event", "kind", e.Kind, "tool", tce.ToolName)
	}
	return nil
}

func (p *DaemonToolCallPublisher) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case req := <-p.queue:
			postCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			if err := p.client.IngestProxyEvent(postCtx, req); err != nil {
				slog.Warn("mcp: recording a tool call with the daemon failed", "tool", req.ToolName, "phase", req.Phase, "err", err)
			}
			cancel()
		}
	}
}
