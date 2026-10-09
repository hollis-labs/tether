//go:build !windows

package shimcodex

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

const ProjectionVersion = "codex-output-v1"
const ProjectionFrameBytes = 64 << 10
const ProjectionBudget = 64 << 20
const ProjectionSources = 1024

// Projection is an immutable candidate, never authentication or delivery proof.
// DeliveredHighWater is inherited only; projection never advances it.
type Projection struct {
	Version                         string              `json:"version"`
	Binding                         Binding             `json:"binding"`
	ProtocolRevision                uint64              `json:"protocol_revision,string"`
	JournalIdentity                 string              `json:"journal_identity"`
	AcceptedSourceCursor            string              `json:"accepted_source_cursor"`
	BaseSourceCursor                string              `json:"base_source_cursor,omitempty"`
	BaseReceiptSHA256               string              `json:"base_receipt_sha256,omitempty"`
	ReplayHighWater                 string              `json:"replay_highwater"`
	DeliveredHighWater              string              `json:"delivered_highwater"`
	StdoutOffset                    uint64              `json:"stdout_offset,string"`
	PartialStart                    uint64              `json:"partial_start,string"`
	PartialBytes                    []byte              `json:"partial_bytes"`
	ActiveThreadID                  string              `json:"active_thread_id"`
	ActiveTurnID                    string              `json:"active_turn_id"`
	Turns                           []ProjectedTurn     `json:"turns"`
	Sources                         []SourceDisposition `json:"source_dispositions"`
	Terminal                        *ProjectedTerminal  `json:"terminal"`
	OutstandingInputIDs             []string            `json:"outstanding_input_ids"`
	OutstandingServerRequestSources []string            `json:"outstanding_server_request_sources"`
}
type ProjectedTurn struct {
	NativeThreadID          string          `json:"native_thread_id"`
	NativeTurnID            string          `json:"native_turn_id"`
	StableOutputTurnID      string          `json:"stable_output_turn_id"`
	Phase                   string          `json:"phase"`
	Items                   []ProjectedItem `json:"items"`
	CompletionSourceID      string          `json:"completion_source_id"`
	CompletionPayloadSHA256 string          `json:"completion_payload_sha256"`
	OutputKind              string          `json:"output_kind"`
	StopReason              string          `json:"stop_reason"`
	OutputAcceptanceID      string          `json:"output_acceptance_id"`
	SelectedRouteDigest     string          `json:"selected_route_digest"`
	OutboxMessageIDs        []string        `json:"outbox_message_ids"`
}
type ProjectedItem struct {
	NativeItemID      string   `json:"native_item_id"`
	Kind              string   `json:"kind"`
	Phase             string   `json:"phase,omitempty"`
	TextBytes         string   `json:"text_bytes"`
	DeltaSourceIDs    []string `json:"delta_source_ids"`
	CompletedSourceID string   `json:"completed_source_id"`
}
type SourceDisposition struct {
	SourceEventID       string   `json:"source_event_id"`
	Cursor              string   `json:"cursor"`
	PayloadSHA256       string   `json:"payload_sha256"`
	Kind                string   `json:"kind"`
	Disposition         string   `json:"disposition"`
	OutputAcceptanceIDs []string `json:"output_acceptance_ids"`
	OutboxMessageIDs    []string `json:"outbox_message_ids"`
}
type ProjectedTerminal struct {
	SourceEventID string `json:"source_event_id"`
	Cursor        string `json:"cursor"`
	PayloadSHA256 string `json:"payload_sha256"`
	Status        int    `json:"status"`
	Signal        string `json:"signal"`
	Cause         string `json:"cause"`
	Disposition   string `json:"disposition"`
}

// JSON's permissive decoder replaces lone UTF-16 surrogates. Refuse that
// semantic loss rather than silently changing provider output bytes.
func projectionUnicodeEscapes(raw []byte) bool {
	inString := false
	for i := 0; i < len(raw); i++ {
		if raw[i] == '"' {
			inString = !inString
			continue
		}
		if !inString || raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) {
			return false
		}
		if raw[i] != 'u' {
			continue
		}
		if i+4 >= len(raw) {
			return false
		}
		n, err := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if n >= 0xdc00 && n <= 0xdfff {
			return false
		}
		if n >= 0xd800 && n <= 0xdbff {
			if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
				return false
			}
			low, err := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		}
	}
	return !inString
}

func strictProjectionJSON(raw []byte, limit int, dst any) error {
	if len(raw) == 0 || len(raw) > limit || !utf8.Valid(raw) || !projectionUnicodeEscapes(raw) || !uniqueJSON(raw) {
		return fail("projection_invalid")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return fail("projection_invalid")
	}
	if d.Decode(new(any)) != io.EOF {
		return fail("projection_invalid")
	}
	return nil
}

// Check collection sizes using streaming tokens before allocating struct slices.
// Each closed projection collection has a bounded identity inventory.
func projectionCollectionBounds(raw []byte) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var walk func(int) bool
	walk = func(depth int) bool {
		if depth > 64 {
			return false
		}
		token, err := d.Token()
		if err != nil {
			return false
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return true
		}
		count := 0
		switch delimiter {
		case '[':
			for d.More() {
				count++
				if count > ProjectionSources || !walk(depth+1) {
					return false
				}
			}
			end, err := d.Token()
			return err == nil && end == json.Delim(']')
		case '{':
			for d.More() {
				count++
				if count > 256 {
					return false
				}
				if _, err := d.Token(); err != nil {
					return false
				}
				if !walk(depth + 1) {
					return false
				}
			}
			end, err := d.Token()
			return err == nil && end == json.Delim('}')
		}
		return false
	}
	return walk(0)
}

func DecodeProjection(raw []byte) (Projection, error) {
	var p Projection
	if len(raw) > ProjectionBudget || !projectionCollectionBounds(raw) {
		return p, fail("projection_invalid")
	}
	if err := strictProjectionJSON(raw, ProjectionBudget, &p); err != nil {
		return p, err
	}
	// encoding/json's ,string accepts noncanonical decimal representations.
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return p, fail("projection_invalid")
	}
	for _, name := range []string{"protocol_revision", "stdout_offset", "partial_start"} {
		if !canonicalCounter(object[name]) {
			return p, fail("projection_invalid")
		}
	}
	var binding map[string]json.RawMessage
	if json.Unmarshal(object["binding"], &binding) != nil || !canonicalCounter(binding["generation"]) {
		return p, fail("projection_invalid")
	}
	return p, ValidateProjection(p)
}
func canonicalCounter(raw []byte) bool {
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return false
	}
	n, err := strconv.ParseUint(text, 10, 64)
	return err == nil && strconv.FormatUint(n, 10) == text
}
func digest(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
func validDigest(text string) bool {
	raw, err := hex.DecodeString(text)
	return err == nil && len(raw) == sha256.Size && hex.EncodeToString(raw) == text
}
func projectedID(binding Binding, parts ...string) string {
	raw, _ := json.Marshal(struct {
		Binding Binding
		Parts   []string
	}{binding, parts})
	return "codex:" + digest(raw)
}
func uniqueStrings(values []string, limit int) bool {
	if len(values) > limit {
		return false
	}
	seen := map[string]bool{}
	for _, v := range values {
		if v == "" || seen[v] {
			return false
		}
		seen[v] = true
	}
	return true
}

func ValidateProjection(p Projection) error {
	if p.Version != ProjectionVersion || !p.Binding.valid() || p.ProtocolRevision == 0 || p.JournalIdentity != p.Binding.Journal || len(p.Sources) > ProjectionSources || len(p.Turns) > MaxOperations || p.PartialStart > p.StdoutOffset || uint64(len(p.PartialBytes)) != p.StdoutOffset-p.PartialStart || !uniqueStrings(p.OutstandingInputIDs, MaxOperations) || !uniqueStrings(p.OutstandingServerRequestSources, MaxOperations) {
		return fail("projection_invalid")
	}
	for _, id := range p.OutstandingInputIDs {
		n, err := strconv.ParseUint(id, 10, 64)
		if err != nil || n < FirstID || n > MaxID || strconv.FormatUint(n, 10) != id {
			return fail("projection_invalid")
		}
	}
	raw, err := json.Marshal(p)
	if err != nil || len(raw) > ProjectionBudget {
		return fail("pressure_retained")
	}
	position := uint64(0)
	if p.BaseSourceCursor != "" {
		position, err = cursorNumber(p.JournalIdentity, p.BaseSourceCursor)
		if err != nil || !validDigest(p.BaseReceiptSHA256) {
			return fail("projection_invalid")
		}
	} else if p.BaseReceiptSHA256 != "" {
		return fail("projection_invalid")
	}
	seen := map[string]SourceDisposition{}
	for _, s := range p.Sources {
		n, err := cursorNumber(p.JournalIdentity, s.Cursor)
		if err != nil || s.SourceEventID == "" || !validDigest(s.PayloadSHA256) || (n < position || position == math.MaxUint64 || n > position+1) || !uniqueStrings(s.OutputAcceptanceIDs, MaxOperations) || !uniqueStrings(s.OutboxMessageIDs, MaxOperations) {
			return fail("projection_invalid")
		}
		if _, ok := seen[s.SourceEventID]; ok {
			return fail("projection_invalid")
		}
		seen[s.SourceEventID] = s
		position = n
		switch s.Kind {
		case "metadata", "response", "notification", "server_request", "provider_exit":
		default:
			return fail("projection_invalid")
		}
		switch s.Disposition {
		case "projected", "protocol_only", "metadata_only", "retained_unsupported":
		default:
			return fail("projection_invalid")
		}
	}
	if p.AcceptedSourceCursor != "" {
		n, err := cursorNumber(p.JournalIdentity, p.AcceptedSourceCursor)
		if err != nil || n != position || len(p.Sources) == 0 && p.AcceptedSourceCursor != p.BaseSourceCursor {
			return fail("projection_invalid")
		}
	} else if len(p.Sources) != 0 {
		return fail("projection_invalid")
	}
	for _, cursor := range []string{p.ReplayHighWater, p.DeliveredHighWater} {
		if cursor != "" {
			if _, err := cursorNumber(p.JournalIdentity, cursor); err != nil {
				return fail("projection_invalid")
			}
		}
	}
	if p.DeliveredHighWater != "" {
		n, _ := cursorNumber(p.JournalIdentity, p.DeliveredHighWater)
		if n > position {
			return fail("projection_invalid")
		}
	}
	turns := map[string]bool{}
	for _, t := range p.Turns {
		if t.NativeThreadID == "" || t.NativeTurnID == "" || turns[t.NativeTurnID] || t.StableOutputTurnID != projectedID(p.Binding, "turn", t.NativeThreadID, t.NativeTurnID) || len(t.Items) > MaxOperations || !uniqueStrings(t.OutboxMessageIDs, MaxOperations) {
			return fail("projection_invalid")
		}
		turns[t.NativeTurnID] = true
		switch t.Phase {
		case "open":
			if t.CompletionSourceID != "" || t.OutputKind != "" {
				return fail("projection_invalid")
			}
		case "completed", "failed", "interrupted":
			s, ok := seen[t.CompletionSourceID]
			if !ok || s.PayloadSHA256 != t.CompletionPayloadSHA256 || t.OutputKind != "final" {
				return fail("projection_invalid")
			}
		default:
			return fail("projection_invalid")
		}
		items := map[string]bool{}
		for _, item := range t.Items {
			if item.NativeItemID == "" || items[item.NativeItemID] || item.Kind != "agent_message" || !uniqueStrings(item.DeltaSourceIDs, ProjectionSources) {
				return fail("projection_invalid")
			}
			items[item.NativeItemID] = true
			for _, id := range item.DeltaSourceIDs {
				if _, ok := seen[id]; !ok {
					return fail("projection_invalid")
				}
			}
			if item.CompletedSourceID != "" {
				if _, ok := seen[item.CompletedSourceID]; !ok {
					return fail("projection_invalid")
				}
			}
		}
	}
	if p.ActiveTurnID != "" && !turns[p.ActiveTurnID] {
		return fail("projection_invalid")
	}
	if p.Terminal != nil {
		s, ok := seen[p.Terminal.SourceEventID]
		if !ok || s.Kind != "provider_exit" || s.PayloadSHA256 != p.Terminal.PayloadSHA256 || s.Cursor != p.Terminal.Cursor || p.Terminal.Disposition != "authenticated_provider_exit" {
			return fail("projection_invalid")
		}
	}
	return nil
}

// ProjectFrozenSource returns a fresh candidate. Continuity is supplied by the
// previous accepted cursor and exact next journal cursor, never caller highwater.
// It does not mutate the protocol inbox, persist, stage, or certify an exit.
func ProjectFrozenSource(previous Projection, event Event) (Projection, error) {
	if err := ValidateProjection(previous); err != nil {
		return Projection{}, err
	}
	raw, _ := json.Marshal(previous)
	next, err := DecodeProjection(raw)
	if err != nil {
		return Projection{}, err
	}
	if _, err := projectFrozenSourceInto(&next, event); err != nil {
		return Projection{}, err
	}
	if err := ValidateProjection(next); err != nil {
		return Projection{}, err
	}
	return next, nil
}

// projectFrozenSourceInto extends a private, validated candidate. Batch callers
// validate the finished candidate once instead of decoding the entire growing
// history for each record. Errors never authorize persistence or inbox removal.
func projectFrozenSourceInto(next *Projection, event Event) (int, error) {
	if len(event.Raw) == 0 || len(event.Raw) > ProjectionFrameBytes || !utf8.Valid(event.Raw) || !projectionUnicodeEscapes(event.Raw) || !uniqueJSON(event.Raw) || event.Identity == "" {
		return -1, fail("projection_invalid")
	}
	hash := digest(event.Raw)
	for index, old := range next.Sources {
		if old.SourceEventID == event.Identity {
			if old.PayloadSHA256 != hash || old.Cursor != event.Cursor {
				return -1, fail("source_conflict")
			}
			return index, nil
		}
	}
	if len(next.Sources) >= ProjectionSources {
		return -1, fail("pressure_retained")
	}
	oldNumber := uint64(0)
	if next.AcceptedSourceCursor != "" {
		oldNumber, _ = cursorNumber(next.JournalIdentity, next.AcceptedSourceCursor)
	}
	nextNumber, err := cursorNumber(next.JournalIdentity, event.Cursor)
	if err != nil || oldNumber == math.MaxUint64 || (nextNumber != oldNumber+1 && nextNumber != oldNumber) {
		return -1, fail("source_gap")
	}
	if strings.HasPrefix(event.Identity, next.JournalIdentity+":stdout:") {
		span := strings.Split(strings.TrimPrefix(event.Identity, next.JournalIdentity+":stdout:"), ":")
		if len(span) != 2 {
			return -1, fail("source_gap")
		}
		start, e1 := strconv.ParseUint(span[0], 10, 64)
		end, e2 := strconv.ParseUint(span[1], 10, 64)
		if e1 != nil || e2 != nil || strconv.FormatUint(start, 10) != span[0] || strconv.FormatUint(end, 10) != span[1] || start != next.StdoutOffset || end <= start || end-start != uint64(len(event.Raw))+1 {
			return -1, fail("source_gap")
		}
		next.StdoutOffset = end
		next.PartialStart = end
	} else if nextNumber == oldNumber {
		return -1, fail("source_gap")
	}
	source := SourceDisposition{SourceEventID: event.Identity, Cursor: event.Cursor, PayloadSHA256: hash, Kind: "notification", Disposition: "retained_unsupported"}
	// Raw metadata and provider exit are closed envelopes produced by the durable
	// reader; a source candidate still cannot authenticate or settle their facts.
	var envelope struct {
		Kind    string          `json:"kind"`
		Payload json.RawMessage `json:"payload"`
	}
	if strictProjectionJSON(event.Raw, ProjectionFrameBytes, &envelope) == nil && envelope.Kind != "" {
		switch envelope.Kind {
		case "shim.pin_adopted", "shim.launch_intent", "shim.started", "shim.attached", "shim.detached", "shim.inject_retry", "shim.inject_intent", "shim.inject_outcome", "shim.refused", "shim.control_intent", "shim.control_outcome", "codex.stdout_fragment":
			if event.Identity != next.JournalIdentity+":"+event.Cursor+":"+envelope.Kind {
				return -1, fail("source_conflict")
			}
			source.Kind = "metadata"
			source.Disposition = "metadata_only"
		case "exit":
			source.Kind = "provider_exit" // authentication remains unavailable here
		}
	} else if event.Identity == next.JournalIdentity+":exit:"+event.Cursor {
		source.Kind = "provider_exit" // Never authenticate from a pure candidate.
	} else if event.Identity == next.JournalIdentity+":"+event.Cursor+":stderr" {
		// Stderr is a retained output obligation, not a protocol response.
	} else {
		m, err := Decode(event.Raw)
		if err != nil {
			return -1, err
		}
		if m.Method == "" {
			source.Kind = "response"
			source.Disposition = "protocol_only"
		} else if len(m.ID) != 0 {
			source.Kind = "server_request"
		} else {
			if err := projectNotification(next, m, event, &source); err != nil {
				return -1, err
			}
		}
	}
	next.Sources = append(next.Sources, source)
	next.AcceptedSourceCursor = event.Cursor
	return len(next.Sources) - 1, nil
}

// supportedProjectionParams classifies closed supported shapes. Other shapes remain retained.
func supportedProjectionParams(raw []byte, dst any) bool {
	return strictProjectionJSON(raw, ProjectionFrameBytes, dst) == nil
}

// Observed app-server additions are explicit here. Unsupported union members
// and future fields continue to retain the original inbox record.
type projectionTurnParams struct {
	ThreadID string `json:"threadId"`
	Turn     struct {
		ID          string          `json:"id"`
		Status      string          `json:"status"`
		Items       json.RawMessage `json:"items,omitempty"`
		ItemsView   json.RawMessage `json:"itemsView,omitempty"`
		Error       json.RawMessage `json:"error,omitempty"`
		StartedAt   json.RawMessage `json:"startedAt,omitempty"`
		CompletedAt json.RawMessage `json:"completedAt,omitempty"`
		DurationMs  json.RawMessage `json:"durationMs,omitempty"`
	} `json:"turn"`
}
type projectionItemParams struct {
	ThreadID      string          `json:"threadId"`
	TurnID        string          `json:"turnId"`
	StartedAtMs   json.RawMessage `json:"startedAtMs,omitempty"`
	CompletedAtMs json.RawMessage `json:"completedAtMs,omitempty"`
	Item          struct {
		ID             string          `json:"id"`
		Type           string          `json:"type"`
		Text           string          `json:"text,omitempty"`
		Phase          string          `json:"phase,omitempty"`
		Delivery       json.RawMessage `json:"delivery,omitempty"`
		MemoryCitation json.RawMessage `json:"memoryCitation,omitempty"`
		Questions      json.RawMessage `json:"questions,omitempty"`
		ClientID       json.RawMessage `json:"clientId,omitempty"`
		Content        json.RawMessage `json:"content,omitempty"`
	} `json:"item"`
}

func projectNotification(p *Projection, m Message, event Event, source *SourceDisposition) error {
	// Unknown fields and unsupported item unions remain retained, never truncated.
	switch m.Method {
	case "remoteControl/status/changed", "account/updated", "thread/started", "mcpServer/startupStatus/updated", "thread/status/changed", "thread/tokenUsage/updated", "account/rateLimits/updated":
		// Status/usage/account notifications are protocol telemetry, never output
		// or grant callbacks. Their bounded original bytes retain a source digest.
		source.Disposition = "protocol_only"
	case "turn/started":
		var params projectionTurnParams
		if !supportedProjectionParams(m.Params, &params) {
			return nil
		}
		if params.ThreadID == "" || params.Turn.ID == "" || params.Turn.Status != "inProgress" {
			return nil
		}
		if p.ActiveThreadID != "" && p.ActiveThreadID != params.ThreadID || p.ActiveTurnID != "" {
			return fail("turn_mismatch")
		}
		for _, t := range p.Turns {
			if t.NativeTurnID == params.Turn.ID {
				return fail("turn_mismatch")
			}
		}
		if len(p.Turns) >= MaxOperations {
			return fail("pressure_retained")
		}
		p.ActiveThreadID = params.ThreadID
		p.ActiveTurnID = params.Turn.ID
		p.Turns = append(p.Turns, ProjectedTurn{NativeThreadID: params.ThreadID, NativeTurnID: params.Turn.ID, StableOutputTurnID: projectedID(p.Binding, "turn", params.ThreadID, params.Turn.ID), Phase: "open"})
		source.Disposition = "projected"
	case "item/started", "item/completed":
		var params projectionItemParams
		if !supportedProjectionParams(m.Params, &params) || params.Item.ID == "" {
			return nil
		}
		if params.Item.Type != "agentMessage" && params.Item.Type != "userMessage" {
			return nil
		}
		t, err := activeProjectedTurn(p, params.ThreadID, params.TurnID)
		if err != nil {
			return err
		}
		if params.Item.Type == "userMessage" {
			source.Disposition = "protocol_only"
			return nil
		}
		if params.Item.Type != "agentMessage" {
			return nil
		}
		index := -1
		for i := range t.Items {
			if t.Items[i].NativeItemID == params.Item.ID {
				index = i
			}
		}
		if m.Method == "item/started" {
			if index != -1 {
				return fail("item_mismatch")
			}
			if len(t.Items) >= MaxOperations {
				return fail("pressure_retained")
			}
			t.Items = append(t.Items, ProjectedItem{NativeItemID: params.Item.ID, Kind: "agent_message", Phase: params.Item.Phase, TextBytes: params.Item.Text})
		} else {
			if index < 0 {
				return fail("item_mismatch")
			}
			item := &t.Items[index]
			if item.CompletedSourceID != "" || item.TextBytes != params.Item.Text || item.Phase != params.Item.Phase {
				return fail("item_mismatch")
			}
			item.CompletedSourceID = event.Identity
		}
		source.Disposition = "projected"
	case "item/agentMessage/delta":
		var params struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
			ItemID   string `json:"itemId"`
			Delta    string `json:"delta"`
		}
		if !supportedProjectionParams(m.Params, &params) {
			return nil
		}
		t, err := activeProjectedTurn(p, params.ThreadID, params.TurnID)
		if err != nil {
			return err
		}
		var item *ProjectedItem
		for i := range t.Items {
			if t.Items[i].NativeItemID == params.ItemID {
				item = &t.Items[i]
			}
		}
		if item == nil || item.CompletedSourceID != "" {
			return fail("item_mismatch")
		}
		if len(params.Delta) > ProjectionBudget-len(item.TextBytes) || len(item.DeltaSourceIDs) >= ProjectionSources {
			return fail("pressure_retained")
		}
		item.TextBytes += params.Delta
		item.DeltaSourceIDs = append(item.DeltaSourceIDs, event.Identity)
		source.Disposition = "projected"
	case "turn/completed":
		var params projectionTurnParams
		if !supportedProjectionParams(m.Params, &params) {
			return nil
		}
		switch params.Turn.Status {
		case "completed", "failed", "interrupted":
		default:
			return nil
		}
		t, err := activeProjectedTurn(p, params.ThreadID, params.Turn.ID)
		if err != nil {
			return err
		}
		for _, item := range t.Items {
			if item.CompletedSourceID == "" {
				return fail("item_pending")
			}
		}
		t.Phase = params.Turn.Status
		t.CompletionSourceID = event.Identity
		t.CompletionPayloadSHA256 = source.PayloadSHA256
		t.OutputKind = "final"
		t.StopReason = params.Turn.Status
		p.ActiveTurnID = ""
		source.Disposition = "projected"
	}
	return nil
}
func activeProjectedTurn(p *Projection, thread, turn string) (*ProjectedTurn, error) {
	if thread == "" || turn == "" || thread != p.ActiveThreadID || turn != p.ActiveTurnID {
		return nil, fail("turn_mismatch")
	}
	for i := range p.Turns {
		if p.Turns[i].NativeTurnID == turn && p.Turns[i].NativeThreadID == thread && p.Turns[i].Phase == "open" {
			return &p.Turns[i], nil
		}
	}
	return nil, fail("turn_mismatch")
}
