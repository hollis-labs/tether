package chatbackend

import (
	"encoding/json"
	"strings"

	chat "github.com/hollis-labs/libs/agent/chat"
	"github.com/hollis-labs/libs/ui/ui-go/chatstream"
)

// Finalizer sits between a decoder and the wire and makes every part.end carry the part's complete content in Final
// (chat-attach-wire.md, Committed-only mode and As built). It is the shared chat.Finalizer from libs/agent/chat
// (CW-20261118-0011) plus the two things a Claude decoder needs on top: a part that arrived whole still gets the
// As-built shape (claudejson's tool_result Final is the vendor block, and a thinking block Claude Code did not display
// has no text), and a non-streaming backend drops its deltas after they were accumulated.
type Finalizer interface {
	// Push takes one decoder event, in order, and returns the events to send in its place: usually the event itself,
	// part.end with Final filled in, or nothing for a part.delta a non-streaming backend drops.
	Push(ev chatstream.Event) []chatstream.Event
}

// FinalizerOptions configure NewFinalizer.
type FinalizerOptions struct {
	// DropDeltas removes part.delta from the output, for a backend whose content arrives whole (Info.Deltas false). The
	// deltas are still accumulated, so Final is complete either way.
	DropDeltas bool
}

// NewFinalizer returns the backends' Finalizer.
//
// What Final holds after it, by part kind (the As-built shapes; an object for every kind):
//
//   - text, refusal: {"text": <the part's text>}.
//   - reasoning: {"text": <the reasoning text>} merged into what the decoder put there ({"signature": ...} or
//     {"data": ...}). Claude Code writes thinking with an empty text when the TUI does not display it; text is then "".
//   - tool_call: the decoder's Final (the parsed arguments); when it has none, the accumulated JSON fragments, else {}.
//   - tool_result: the decoder's Final (the vendor block) with "text" (the result's text content, flattened) and
//     "is_error" (a bool) added.
//   - anything else: left as the decoder sent it.
func NewFinalizer(o FinalizerOptions) Finalizer {
	return &finalizer{opts: o, parts: map[string]*partInfo{}}
}

type finalizer struct {
	opts  FinalizerOptions
	fin   chat.Finalizer
	parts map[string]*partInfo
}

// partInfo is what the shape fix-ups need beyond chat.Finalizer's own accumulation.
type partInfo struct {
	kind    chatstream.PartKind
	isError bool
	text    strings.Builder
}

func (f *finalizer) Push(ev chatstream.Event) []chatstream.Event {
	switch ev.Verb {
	case chatstream.VerbPartStart:
		f.parts[ev.PartID] = &partInfo{kind: ev.PartKind(), isError: ev.MetaBool(chatstream.MetaIsError)}
	case chatstream.VerbPartDelta:
		if p := f.parts[ev.PartID]; p != nil {
			p.text.WriteString(ev.Text)
		}
	case chatstream.VerbPartEnd:
		p := f.parts[ev.PartID]
		delete(f.parts, ev.PartID)
		ev = f.fin.Apply(ev)
		if p != nil {
			ev.Final = p.shape(ev.Final)
		}
		return []chatstream.Event{ev}
	default:
	}
	ev = f.fin.Apply(ev)
	if ev.Verb == chatstream.VerbPartDelta && f.opts.DropDeltas {
		return nil
	}
	return []chatstream.Event{ev}
}

// shape brings a part.end's Final, after chat.Finalizer, to the As-built shape for parts the shared finalizer leaves
// alone: those that arrived whole, or whose deltas carried no text.
func (p *partInfo) shape(have json.RawMessage) json.RawMessage {
	switch p.kind {
	case chatstream.PartText, chatstream.PartRefusal, chatstream.PartReasoning:
		if chat.HasFinal(p.kind, have) {
			return have
		}
		return mergeObject(have, map[string]any{"text": p.text.String()})
	case chatstream.PartToolCall:
		if len(have) > 0 {
			return have
		}
		return json.RawMessage("{}")
	case chatstream.PartToolResult:
		var block struct {
			Content json.RawMessage `json:"content"`
			IsError *bool           `json:"is_error"`
			Text    *string         `json:"text"`
		}
		_ = json.Unmarshal(have, &block)
		set := map[string]any{}
		if block.Text == nil {
			text := flattenContent(block.Content)
			if text == "" {
				text = p.text.String()
			}
			set["text"] = text
		}
		isErr := p.isError || (block.IsError != nil && *block.IsError)
		if block.IsError == nil || isErr != *block.IsError {
			set["is_error"] = isErr
		}
		if len(set) == 0 {
			return have
		}
		return mergeObject(have, set)
	default:
		return have
	}
}

// mergeObject sets keys on a JSON object, keeping its other fields. A Final that is not an object is kept under "value".
func mergeObject(have json.RawMessage, set map[string]any) json.RawMessage {
	obj := map[string]json.RawMessage{}
	if len(have) > 0 && json.Unmarshal(have, &obj) != nil {
		obj = map[string]json.RawMessage{"value": have}
	}
	if obj == nil {
		obj = map[string]json.RawMessage{}
	}
	for k, v := range set {
		b, err := json.Marshal(v)
		if err != nil {
			continue
		}
		obj[k] = b
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return have
	}
	return out
}

// flattenContent is the text of a tool result's content: a string, or the text blocks of an array joined by newlines.
func flattenContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}
