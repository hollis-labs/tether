//go:build !windows

package shimcodex

import (
	"bytes"
	"encoding/json"
)

// ProjectedCommand retains a private turn obligation, not final reply text or an
// approval capability. Byte slices preserve the complete native item envelopes
// through checkpoint JSON round trips, including output snapshots and metadata.
type ProjectedCommand struct {
	StartedParams   []byte                         `json:"started_params"`
	CompletedParams []byte                         `json:"completed_params,omitempty"`
	Interactions    []ProjectedTerminalInteraction `json:"interactions,omitempty"`
}

// ProjectedTerminalInteraction records observed native command input, never
// authorization to write stdin. Its exact private bytes stay out of the reply.
type ProjectedTerminalInteraction struct {
	SourceEventID string `json:"source_event_id"`
	Params        []byte `json:"params"`
}

type projectionCommandParams struct {
	ThreadID      string          `json:"threadId"`
	TurnID        string          `json:"turnId"`
	StartedAtMs   json.RawMessage `json:"startedAtMs,omitempty"`
	CompletedAtMs json.RawMessage `json:"completedAtMs,omitempty"`
	Item          struct {
		ID               string            `json:"id"`
		Type             string            `json:"type"`
		Command          string            `json:"command"`
		Cwd              string            `json:"cwd"`
		CommandActions   []json.RawMessage `json:"commandActions"`
		Source           string            `json:"source,omitempty"`
		Status           string            `json:"status"`
		AggregatedOutput *string           `json:"aggregatedOutput,omitempty"`
		DurationMs       *int64            `json:"durationMs,omitempty"`
		ExitCode         *int32            `json:"exitCode,omitempty"`
		ProcessID        *string           `json:"processId,omitempty"`
		PluginID         *string           `json:"pluginId,omitempty"`
		ScriptPath       *string           `json:"scriptPath,omitempty"`
	} `json:"item"`
}

func decodeCommandParams(raw []byte) (projectionCommandParams, bool) {
	var p projectionCommandParams
	if !supportedProjectionParams(raw, &p) || p.ThreadID == "" || p.TurnID == "" || p.Item.ID == "" || p.Item.Type != "commandExecution" || p.Item.Command == "" || p.Item.Cwd == "" || p.Item.CommandActions == nil {
		return p, false
	}
	switch p.Item.Source {
	case "", "agent", "userShell", "unifiedExecStartup", "unifiedExecInteraction":
	default:
		return p, false
	}
	switch p.Item.Status {
	case "inProgress", "completed", "failed", "declined":
	default:
		return p, false
	}
	return p, true
}

func commandIdentityMatches(a, b projectionCommandParams) bool {
	aa, _ := json.Marshal(a.Item.CommandActions)
	bb, _ := json.Marshal(b.Item.CommandActions)
	eq := func(x, y *string) bool { return x == nil && y == nil || x != nil && y != nil && *x == *y }
	process := a.Item.ProcessID == nil || b.Item.ProcessID != nil && *a.Item.ProcessID == *b.Item.ProcessID
	return a.ThreadID == b.ThreadID && a.TurnID == b.TurnID && a.Item.ID == b.Item.ID && a.Item.Command == b.Item.Command && a.Item.Cwd == b.Item.Cwd && a.Item.Source == b.Item.Source && eq(a.Item.PluginID, b.Item.PluginID) && eq(a.Item.ScriptPath, b.Item.ScriptPath) && process && bytes.Equal(aa, bb)
}

func projectCommandItem(p *Projection, m Message, event Event, source *SourceDisposition) error {
	params, ok := decodeCommandParams(m.Params)
	if !ok {
		return nil
	} // the original record remains retained_unsupported
	t, err := activeProjectedTurn(p, params.ThreadID, params.TurnID)
	if err != nil {
		return err
	}
	index := -1
	for i := range t.Items {
		if t.Items[i].NativeItemID == params.Item.ID {
			index = i
		}
	}
	if m.Method == "item/started" {
		if params.Item.Status != "inProgress" {
			return nil
		}
		if index >= 0 {
			return fail("item_mismatch")
		}
		if len(t.Items) >= MaxOperations {
			return fail("pressure_retained")
		}
		text := ""
		if params.Item.AggregatedOutput != nil {
			text = *params.Item.AggregatedOutput
		}
		t.Items = append(t.Items, ProjectedItem{NativeItemID: params.Item.ID, Kind: "command_execution", Phase: params.Item.Status, TextBytes: text, StartedSourceID: event.Identity, Command: &ProjectedCommand{StartedParams: append([]byte(nil), m.Params...)}})
	} else {
		if params.Item.Status == "inProgress" {
			return nil
		}
		if index < 0 {
			return fail("item_mismatch")
		}
		item := &t.Items[index]
		if item.Kind != "command_execution" || item.Command == nil || item.CompletedSourceID != "" {
			return fail("item_mismatch")
		}
		start, ok := decodeCommandParams(item.Command.StartedParams)
		if !ok || !commandIdentityMatches(start, params) {
			return fail("item_mismatch")
		}
		// Native completion may include an aggregate even when streaming was
		// omitted or truncated. Preserve it independently of the exact delta bytes;
		// neither is concatenated into the agent's public final reply.
		if len(item.DeltaSourceIDs) == 0 && params.Item.AggregatedOutput != nil {
			item.TextBytes = *params.Item.AggregatedOutput
		}
		item.Command.CompletedParams = append([]byte(nil), m.Params...)
		item.CompletedSourceID = event.Identity
		item.Phase = params.Item.Status
	}
	source.Disposition = "projected"
	return nil
}

func validateProjectedCommand(t ProjectedTurn, item ProjectedItem, sources map[string]SourceDisposition) error {
	if item.Command == nil || item.StartedSourceID == "" {
		return fail("projection_invalid")
	}
	source, exists := sources[item.StartedSourceID]
	start, ok := decodeCommandParams(item.Command.StartedParams)
	if !exists || source.Kind != "notification" || source.Disposition != "projected" || !ok || start.ThreadID != t.NativeThreadID || start.TurnID != t.NativeTurnID || start.Item.ID != item.NativeItemID || start.Item.Status != "inProgress" {
		return fail("projection_invalid")
	}
	if len(item.Command.Interactions) > ProjectionSources {
		return fail("projection_invalid")
	}
	interactionIDs := make([]string, 0, len(item.Command.Interactions))
	for _, interaction := range item.Command.Interactions {
		params, ok := decodeTerminalInteraction(interaction.Params)
		source, exists := sources[interaction.SourceEventID]
		if !ok || !exists || source.Kind != "notification" || source.Disposition != "projected" || !terminalInteractionMatches(start, params) {
			return fail("projection_invalid")
		}
		interactionIDs = append(interactionIDs, interaction.SourceEventID)
	}
	if !uniqueStrings(interactionIDs, ProjectionSources) {
		return fail("projection_invalid")
	}
	if item.CompletedSourceID == "" {
		if len(item.Command.CompletedParams) != 0 || item.Phase != "inProgress" {
			return fail("projection_invalid")
		}
		return nil
	}
	complete, ok := decodeCommandParams(item.Command.CompletedParams)
	source, exists = sources[item.CompletedSourceID]
	if !exists || source.Kind != "notification" || source.Disposition != "projected" || !ok || complete.Item.Status == "inProgress" || complete.Item.Status != item.Phase || !commandIdentityMatches(start, complete) {
		return fail("projection_invalid")
	}
	return nil
}
