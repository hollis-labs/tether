//go:build !windows

package shimcodex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/hollis-labs/substrate/harness/shim"
)

type Message struct {
	ID     json.RawMessage
	Method string
	Params json.RawMessage
	Result json.RawMessage
	Error  *RPCError
}

// Decode distinguishes responses from provider requests even when both have
// numeric IDs. Duplicate fields are refused rather than interpreted twice.
func Decode(raw []byte) (Message, error) {
	var m Message
	if len(raw) > MaxLineBytes {
		return m, fail("line_too_long")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	t, err := d.Token()
	if err != nil || t != json.Delim('{') {
		return m, fail("protocol_invalid")
	}
	fields := make(map[string]json.RawMessage)
	for d.More() {
		t, err = d.Token()
		if err != nil {
			return m, fail("protocol_invalid")
		}
		key, ok := t.(string)
		if !ok {
			return m, fail("protocol_invalid")
		}
		if _, duplicate := fields[key]; duplicate {
			return m, fail("protocol_invalid")
		}
		if len(fields) >= 8 {
			return m, fail("protocol_invalid")
		}
		var v json.RawMessage
		if d.Decode(&v) != nil {
			return m, fail("protocol_invalid")
		}
		fields[key] = v
	}
	if t, err = d.Token(); err != nil || t != json.Delim('}') {
		return m, fail("protocol_invalid")
	}
	if d.Decode(new(any)) != io.EOF {
		return m, fail("protocol_invalid")
	}
	if version, ok := fields["jsonrpc"]; ok {
		var v string
		if json.Unmarshal(version, &v) != nil || v != "2.0" {
			return m, fail("protocol_invalid")
		}
	}
	m.ID = fields["id"]
	if len(m.ID) != 0 && bytes.Equal(bytes.TrimSpace(m.ID), []byte("null")) {
		return m, fail("protocol_invalid")
	}
	if method, ok := fields["method"]; ok {
		if json.Unmarshal(method, &m.Method) != nil || m.Method == "" {
			return m, fail("protocol_invalid")
		}
	}
	m.Params, m.Result = fields["params"], fields["result"]
	errorRaw, hasError := fields["error"]
	if hasError {
		var f struct {
			Code    *int            `json:"code"`
			Message *string         `json:"message"`
			Data    json.RawMessage `json:"data"`
		}
		if json.Unmarshal(errorRaw, &f) != nil || f.Code == nil || f.Message == nil {
			return m, fail("protocol_invalid")
		}
		m.Error = &RPCError{Code: *f.Code, Message: *f.Message, Data: f.Data}
	}
	if m.Method != "" {
		if len(m.Result) != 0 || hasError {
			return m, fail("protocol_invalid")
		}
		if len(m.ID) != 0 {
			var text string
			if json.Unmarshal(m.ID, &text) == nil {
				if text == "" || len(text) > 256 {
					return m, fail("protocol_invalid")
				}
			} else {
				n, err := strconv.ParseUint(string(m.ID), 10, 64)
				if err != nil || n > MaxID || strconv.FormatUint(n, 10) != string(m.ID) {
					return m, fail("protocol_invalid")
				}
			}
		}
		return m, nil
	}
	if len(m.ID) == 0 || (len(m.Result) != 0) == hasError {
		return m, fail("protocol_invalid")
	}
	return m, nil
}

func numericID(raw json.RawMessage) (uint64, error) {
	id, err := strconv.ParseUint(string(raw), 10, 64)
	if err != nil || id < FirstID || id > MaxID || strconv.FormatUint(id, 10) != string(raw) {
		return 0, fail("protocol_invalid")
	}
	return id, nil
}

func applyMessage(s *State, raw []byte, source string) error {
	m, err := Decode(raw)
	if err != nil {
		return err
	}
	if m.Method != "" {
		if len(m.ID) != 0 {
			for _, prior := range s.ServerRequests {
				if prior.Source == source {
					return nil
				}
				if bytes.Equal(prior.ID, m.ID) && !prior.Written {
					return fail("server_request_conflict")
				}
			}
			if len(s.ServerRequests) >= MaxOperations {
				return fail("pressure_retained")
			}
			s.ServerRequests = append(s.ServerRequests, ServerRequest{Source: source, ID: append(json.RawMessage(nil), m.ID...), Method: m.Method, Params: append(json.RawMessage(nil), m.Params...)})
			return nil
		}
		if m.Method != "turn/started" && m.Method != "turn/completed" {
			return nil
		}
		var p struct {
			ThreadID string `json:"threadId"`
			Turn     struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"turn"`
		}
		if json.Unmarshal(m.Params, &p) != nil || p.ThreadID == "" || p.ThreadID != s.ThreadID || p.Turn.ID == "" {
			return fail("turn_mismatch")
		}
		if m.Method == "turn/started" {
			if s.ActiveTurn != "" && s.ActiveTurn != p.Turn.ID {
				return fail("turn_mismatch")
			}
			if p.Turn.ID == s.LastTerminal {
				return fail("turn_mismatch")
			}
			s.ActiveTurn = p.Turn.ID
		} else {
			if p.Turn.ID != s.ActiveTurn {
				if p.Turn.ID == s.LastTerminal {
					return nil
				}
				return fail("turn_mismatch")
			}
			if p.Turn.Status != "completed" && p.Turn.Status != "failed" && p.Turn.Status != "interrupted" {
				return fail("protocol_invalid")
			}
			s.LastTerminal, s.ActiveTurn = p.Turn.ID, ""
		}
		return nil
	}
	id, err := numericID(m.ID)
	if err != nil {
		return err
	}
	op := find(s, id)
	if op == nil {
		if id >= s.NextID {
			return fail("response_identity_mismatch")
		}
		return nil // old settled/evicted ID, never a fresh call
	}
	if op.Notification || op.Phase == Intent || op.Phase == NotSubmitted {
		return fail("response_identity_mismatch")
	}
	if op.Phase == Answered {
		a, _ := json.Marshal(op.RPCError)
		b, _ := json.Marshal(m.Error)
		if !bytes.Equal(bytes.TrimSpace(op.Result), bytes.TrimSpace(m.Result)) || !bytes.Equal(a, b) {
			return fail("response_conflict")
		}
		return nil
	}
	if m.Error == nil {
		switch op.Method {
		case "initialize":
			if id != s.InitializeID {
				return fail("response_identity_mismatch")
			}
			var obj struct {
				UserAgent string `json:"userAgent"`
			}
			if json.Unmarshal(m.Result, &obj) != nil || bytes.Equal(bytes.TrimSpace(m.Result), []byte("null")) {
				return fail("protocol_invalid")
			}
		case "thread/start":
			var r struct {
				Thread struct {
					ID string `json:"id"`
				} `json:"thread"`
			}
			if json.Unmarshal(m.Result, &r) != nil || r.Thread.ID == "" || s.ThreadID != "" && s.ThreadID != r.Thread.ID {
				return fail("thread_mismatch")
			}
			s.ThreadID = r.Thread.ID
		case "turn/start":
			var r struct {
				Turn struct {
					ID string `json:"id"`
				} `json:"turn"`
			}
			if json.Unmarshal(m.Result, &r) != nil || r.Turn.ID == "" || s.ActiveTurn != "" && s.ActiveTurn != r.Turn.ID {
				return fail("turn_mismatch")
			}
			// Notifications can complete the turn before its response arrives.
			if s.LastTerminal != r.Turn.ID {
				s.ActiveTurn = r.Turn.ID
			}
		case "turn/steer":
			var r struct {
				TurnID string `json:"turnId"`
			}
			var p struct {
				Expected string `json:"expectedTurnId"`
			}
			if json.Unmarshal(m.Result, &r) != nil || json.Unmarshal(op.Params, &p) != nil || r.TurnID == "" || r.TurnID != p.Expected {
				return fail("turn_mismatch")
			}
		case "turn/interrupt":
			var r map[string]json.RawMessage
			if json.Unmarshal(m.Result, &r) != nil || r == nil {
				return fail("protocol_invalid")
			}
		}
	}
	op.Phase, op.Result, op.RPCError = Answered, append(json.RawMessage(nil), m.Result...), m.Error
	if m.Error != nil {
		op.EffectUnknown = true
	}
	return nil
}

func cursorNumber(journal, cursor string) (uint64, error) {
	prefix := journal + ":"
	if !strings.HasPrefix(cursor, prefix) {
		return 0, fail("cursor_mismatch")
	}
	x := strings.TrimPrefix(cursor, prefix)
	n, err := strconv.ParseUint(x, 10, 64)
	if err != nil || strconv.FormatUint(n, 10) != x {
		return 0, fail("cursor_mismatch")
	}
	return n, nil
}

// AcceptOutput commits carry, protocol transitions, inbox and cursor together.
// The caller ACKs only after success. An incomplete stdout line remains private
// durable carry; it is not emitted as an allegedly complete protocol message.
func (e *Engine) AcceptOutput(ctx context.Context, epoch uint64, cursor, stream string, data []byte) (bool, error) {
	if len(data) > shim.OutputChunk {
		return false, fail("frame_too_large")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	accepted := false
	err := e.commitLocked(ctx, func(s *State) error {
		if epoch != s.Epoch {
			return fail("epoch_mismatch")
		}
		position, err := cursorNumber(s.Binding.Journal, cursor)
		if err != nil {
			return err
		}
		if s.Cursor != "" {
			previous, err := cursorNumber(s.Binding.Journal, s.Cursor)
			if err != nil {
				return err
			}
			if position <= previous {
				return nil
			}
		}
		if stream != "stdout" && stream != "stderr" {
			return fail("protocol_invalid")
		}
		if stream == "stderr" {
			raw, _ := json.Marshal(struct {
				Stream string `json:"stream"`
				Data   []byte `json:"data"`
			}{stream, data})
			s.Inbox = append(s.Inbox, Event{Identity: s.Binding.Journal + ":" + cursor + ":stderr", Cursor: cursor, Raw: raw})
		} else {
			if uint64(len(data)) > ^uint64(0)-s.StreamOffset {
				return fail("counter_exhausted")
			}
			if len(s.Partial) == 0 {
				s.PartialStart = s.StreamOffset
			}
			s.StreamOffset += uint64(len(data))
			s.Partial = append(s.Partial, data...)
			for {
				i := bytes.IndexByte(s.Partial, '\n')
				if i < 0 {
					break
				}
				if i > MaxLineBytes {
					return fail("line_too_long")
				}
				line := s.Partial[:i]
				end := s.PartialStart + uint64(i+1)
				identity := fmt.Sprintf("%s:stdout:%d:%d", s.Binding.Journal, s.PartialStart, end)
				if err := applyMessage(s, line, identity); err != nil {
					return err
				}
				s.Inbox = append(s.Inbox, Event{Identity: identity, Cursor: cursor, Raw: append(json.RawMessage(nil), line...)})
				s.PartialStart = end
				s.Partial = s.Partial[i+1:]
			}
			if len(s.Partial) > MaxLineBytes {
				return fail("line_too_long")
			}
		}
		bytesUsed := len(s.Partial)
		for _, event := range s.Inbox {
			if len(event.Raw) > e.limits.InboxBytes-bytesUsed {
				return fail("pressure_retained")
			}
			bytesUsed += len(event.Raw)
		}
		if len(s.Inbox) > e.limits.InboxItems || bytesUsed > e.limits.InboxBytes {
			return fail("pressure_retained")
		}
		s.Cursor = cursor
		accepted = true
		return nil
	})
	return accepted, err
}

func (e *Engine) AcceptExit(ctx context.Context, cursor string, exit shim.Exit) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.commitLocked(ctx, func(s *State) error {
		n, err := cursorNumber(s.Binding.Journal, cursor)
		if err != nil {
			return err
		}
		if s.Cursor != "" {
			old, err := cursorNumber(s.Binding.Journal, s.Cursor)
			if err != nil {
				return err
			}
			if n <= old {
				return nil
			}
		}
		if s.Exit != nil && *s.Exit != exit {
			return fail("exit_conflict")
		}
		raw, _ := json.Marshal(exit)
		if len(s.Inbox) >= e.limits.InboxItems {
			return fail("pressure_retained")
		}
		used := len(s.Partial) + len(raw)
		for _, ev := range s.Inbox {
			if len(ev.Raw) > e.limits.InboxBytes-used {
				return fail("pressure_retained")
			}
			used += len(ev.Raw)
		}
		if used > e.limits.InboxBytes {
			return fail("pressure_retained")
		}
		s.Exit = &exit
		s.ExitCursor = cursor
		s.Cursor = cursor
		s.Inbox = append(s.Inbox, Event{Identity: s.Binding.Journal + ":exit:" + cursor, Cursor: cursor, Raw: raw})
		return nil
	})
}

// AcceptMetadata preserves authenticated non-output journal entries in the
// same private inbox/cursor transaction. Skipping them would falsely leave
// replay behind its highwater or require a lossy cursor jump.
func (e *Engine) AcceptMetadata(ctx context.Context, epoch uint64, cursor, kind string, payload json.RawMessage) error {
	switch kind {
	case "shim.pin_adopted", "shim.launch_intent", "shim.started", "shim.attached", "shim.detached", "shim.inject_retry", "shim.inject_intent", "shim.inject_outcome", "shim.refused", "shim.control_intent", "shim.control_outcome":
	default:
		return fail("protocol_unsupported")
	}
	if !json.Valid(payload) || len(payload) > shim.MaxFrame {
		return fail("protocol_invalid")
	}
	raw, err := json.Marshal(struct {
		Kind    string          `json:"kind"`
		Payload json.RawMessage `json:"payload"`
	}{kind, payload})
	if err != nil {
		return fail("protocol_invalid")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.commitLocked(ctx, func(s *State) error {
		if epoch != s.Epoch {
			return fail("epoch_mismatch")
		}
		n, err := cursorNumber(s.Binding.Journal, cursor)
		if err != nil {
			return err
		}
		if s.Cursor != "" {
			old, err := cursorNumber(s.Binding.Journal, s.Cursor)
			if err != nil {
				return err
			}
			if n <= old {
				return nil
			}
		}
		if len(s.Inbox) >= e.limits.InboxItems {
			return fail("pressure_retained")
		}
		used := len(s.Partial)
		for _, ev := range s.Inbox {
			if len(ev.Raw) > e.limits.InboxBytes-used {
				return fail("pressure_retained")
			}
			used += len(ev.Raw)
		}
		if len(raw) > e.limits.InboxBytes-used {
			return fail("pressure_retained")
		}
		s.Inbox = append(s.Inbox, Event{Identity: s.Binding.Journal + ":" + cursor + ":" + kind, Cursor: cursor, Raw: raw})
		s.Cursor = cursor
		return nil
	})
}
