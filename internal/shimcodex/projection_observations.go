//go:build !windows

package shimcodex

import "encoding/json"

// Empty native reasoning has no text to publish, but its unfinished lifecycle
// remains a turn obligation. Nonempty reasoning is deliberately unsupported.
func projectEmptyReasoning(p *Projection, m Message, event Event, source *SourceDisposition) error {
	var params struct {
		ThreadID      string          `json:"threadId"`
		TurnID        string          `json:"turnId"`
		StartedAtMs   json.RawMessage `json:"startedAtMs,omitempty"`
		CompletedAtMs json.RawMessage `json:"completedAtMs,omitempty"`
		Item          struct {
			ID      string            `json:"id"`
			Type    string            `json:"type"`
			Summary []json.RawMessage `json:"summary"`
			Content []json.RawMessage `json:"content"`
		} `json:"item"`
	}
	if !supportedProjectionParams(m.Params, &params) || params.Item.ID == "" || params.Item.Type != "reasoning" || params.Item.Summary == nil || params.Item.Content == nil || len(params.Item.Summary) != 0 || len(params.Item.Content) != 0 {
		return nil
	}
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
		if index >= 0 {
			return fail("item_mismatch")
		}
		if len(t.Items) >= MaxOperations {
			return fail("pressure_retained")
		}
		t.Items = append(t.Items, ProjectedItem{NativeItemID: params.Item.ID, Kind: "empty_reasoning", Phase: "open", StartedSourceID: event.Identity})
	} else {
		if index < 0 {
			return fail("item_mismatch")
		}
		item := &t.Items[index]
		if item.Kind != "empty_reasoning" || item.CompletedSourceID != "" {
			return fail("item_mismatch")
		}
		item.CompletedSourceID = event.Identity
		item.Phase = "completed"
	}
	source.Disposition = "projected"
	return nil
}

func validateEmptyReasoning(item ProjectedItem, sources map[string]SourceDisposition) error {
	start, ok := sources[item.StartedSourceID]
	if !ok || start.Kind != "notification" || start.Disposition != "projected" || item.Command != nil || item.TextBytes != "" || len(item.DeltaSourceIDs) != 0 {
		return fail("projection_invalid")
	}
	if item.CompletedSourceID == "" {
		if item.Phase != "open" {
			return fail("projection_invalid")
		}
	} else {
		end, ok := sources[item.CompletedSourceID]
		if !ok || end.Kind != "notification" || end.Disposition != "projected" || item.Phase != "completed" || item.CompletedSourceID == item.StartedSourceID {
			return fail("projection_invalid")
		}
	}
	return nil
}

type terminalInteractionParams struct {
	ThreadID  string  `json:"threadId"`
	TurnID    string  `json:"turnId"`
	ItemID    string  `json:"itemId"`
	ProcessID string  `json:"processId"`
	Stdin     *string `json:"stdin"`
}

func decodeTerminalInteraction(raw []byte) (terminalInteractionParams, bool) {
	var params terminalInteractionParams
	ok := supportedProjectionParams(raw, &params) && params.ThreadID != "" && params.TurnID != "" && params.ItemID != "" && params.ProcessID != "" && params.Stdin != nil
	return params, ok
}

func terminalInteractionMatches(start projectionCommandParams, params terminalInteractionParams) bool {
	return start.ThreadID == params.ThreadID && start.TurnID == params.TurnID && start.Item.ID == params.ItemID && start.Item.ProcessID != nil && *start.Item.ProcessID == params.ProcessID
}

func projectTerminalInteraction(p *Projection, m Message, event Event, source *SourceDisposition) error {
	params, ok := decodeTerminalInteraction(m.Params)
	if !ok {
		return nil
	}
	t, err := activeProjectedTurn(p, params.ThreadID, params.TurnID)
	if err != nil {
		return err
	}
	for i := range t.Items {
		item := &t.Items[i]
		if item.NativeItemID != params.ItemID {
			continue
		}
		if item.Kind != "command_execution" || item.Command == nil || item.CompletedSourceID != "" {
			return fail("item_mismatch")
		}
		start, ok := decodeCommandParams(item.Command.StartedParams)
		if !ok || !terminalInteractionMatches(start, params) {
			return fail("item_mismatch")
		}
		if len(item.Command.Interactions) >= ProjectionSources {
			return fail("pressure_retained")
		}
		item.Command.Interactions = append(item.Command.Interactions, ProjectedTerminalInteraction{SourceEventID: event.Identity, Params: append([]byte(nil), m.Params...)})
		source.Disposition = "projected"
		return nil
	}
	return fail("item_mismatch")
}
