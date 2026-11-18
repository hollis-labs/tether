package chatbackend

import (
	"encoding/json"
	"strings"

	"github.com/hollis-labs/libs/ui/ui-go/chatstream"
)

// Finalizer sits between a decoder and the wire and makes every part.end carry the part's complete content in Final
// (chat-attach-wire.md, Committed-only mode). A decoder does not always do that itself: claudejson sets Final on
// tool_call (the arguments) and tool_result (the block), but not on text or reasoning. It is an interface so the shared
// finalizer planned for libs/agent/chat can replace this one without touching the backends.
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

// NewFinalizer returns the local Finalizer.
//
// What Final holds after it, by part kind (an object for every kind, so a committed-only client reads one shape):
//
//   - text, refusal: {"text": <the part's text>}.
//   - reasoning: {"text": <the reasoning text>} merged into what the decoder put there ({"signature": ...} or
//     {"data": ...}). Claude Code writes thinking with an empty text when the TUI does not display it; text is then "".
//   - tool_call: the decoder's Final (the parsed arguments); when it has none, the accumulated JSON fragments, else {}.
//   - tool_result: the decoder's Final (the vendor block) with "text" (the result's text content, flattened) and
//     "is_error" (a bool) added.
//   - anything else: left as the decoder sent it.
func NewFinalizer(o FinalizerOptions) Finalizer {
	return &finalizer{opts: o, parts: map[string]*partAcc{}}
}

type finalizer struct {
	opts  FinalizerOptions
	parts map[string]*partAcc
}

type partAcc struct {
	kind    chatstream.PartKind
	text    strings.Builder
	args    strings.Builder
	isError bool
}

func (f *finalizer) Push(ev chatstream.Event) []chatstream.Event {
	switch ev.Verb {
	case chatstream.VerbPartStart:
		acc := &partAcc{kind: ev.PartKind()}
		if raw, ok := ev.Meta[chatstream.MetaIsError]; ok && string(raw) == "true" {
			acc.isError = true
		}
		f.parts[ev.PartID] = acc
	case chatstream.VerbPartDelta:
		if acc := f.parts[ev.PartID]; acc != nil {
			acc.text.WriteString(ev.Text)
			acc.args.WriteString(ev.JSONFragment)
		}
		if f.opts.DropDeltas {
			return nil
		}
	case chatstream.VerbPartEnd:
		if acc := f.parts[ev.PartID]; acc != nil {
			ev.Final = acc.final(ev.Final)
			delete(f.parts, ev.PartID)
		}
	default:
	}
	return []chatstream.Event{ev}
}

func (a *partAcc) final(have json.RawMessage) json.RawMessage {
	switch a.kind {
	case chatstream.PartText, chatstream.PartRefusal:
		return mergeObject(have, map[string]any{"text": a.text.String()})
	case chatstream.PartReasoning:
		return mergeObject(have, map[string]any{"text": a.text.String()})
	case chatstream.PartToolCall:
		if len(have) > 0 {
			return have
		}
		if s := strings.TrimSpace(a.args.String()); s != "" && json.Valid([]byte(s)) {
			return json.RawMessage(s)
		}
		return json.RawMessage("{}")
	case chatstream.PartToolResult:
		var block struct {
			Content json.RawMessage `json:"content"`
			IsError *bool           `json:"is_error"`
		}
		_ = json.Unmarshal(have, &block)
		isErr := a.isError || (block.IsError != nil && *block.IsError)
		text := flattenContent(block.Content)
		if text == "" {
			text = a.text.String()
		}
		return mergeObject(have, map[string]any{"text": text, "is_error": isErr})
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
