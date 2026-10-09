//go:build !windows

package shimcodex

import "encoding/json"

// Validate loaded records before cloning: malformed JSON values must not turn
// an uncertain committed ledger into an empty fresh session through clone.
func validateState(s State, limits Limits) error {
	if s.Delivery != nil && (s.Delivery.Binding != s.Binding || s.Delivery.ProtocolRevision > s.Revision || ValidateProjection(*s.Delivery) != nil) {
		return fail("checkpoint_invalid")
	}
	if s.Version != Version || !s.Binding.valid() || s.Revision == 0 || s.Epoch == 0 || s.NextID < FirstID || s.NextID > MaxID+1 || len(s.Operations) > MaxOperations || len(s.ServerRequests) > MaxOperations || len(s.Partial) > MaxLineBytes || len(s.Inbox) > limits.InboxItems {
		return fail("checkpoint_invalid")
	}
	if _, err := json.Marshal(s); err != nil {
		return fail("checkpoint_invalid")
	}
	if s.PartialStart > s.StreamOffset || uint64(len(s.Partial)) != s.StreamOffset-s.PartialStart {
		return fail("checkpoint_invalid")
	}
	if s.InitializeID != 0 && (s.InitializeID < FirstID || s.InitializeID >= s.NextID) || s.Initialized && s.InitializeID == 0 || s.ThreadID != "" && !s.Initialized || s.ActiveTurn != "" && s.ThreadID == "" {
		return fail("checkpoint_invalid")
	}
	if s.Cursor != "" {
		if _, err := cursorNumber(s.Binding.Journal, s.Cursor); err != nil {
			return fail("checkpoint_invalid")
		}
	}
	if s.ReplayHighWater != "" {
		if _, err := cursorNumber(s.Binding.Journal, s.ReplayHighWater); err != nil {
			return fail("checkpoint_invalid")
		}
	}
	if s.Exit != nil {
		if s.ExitCursor == "" {
			return fail("checkpoint_invalid")
		}
		if _, err := cursorNumber(s.Binding.Journal, s.ExitCursor); err != nil {
			return fail("checkpoint_invalid")
		}
	}
	ids := map[uint64]bool{}
	for _, op := range s.Operations {
		if op.ID < FirstID || op.ID >= s.NextID || ids[op.ID] || !json.Valid(op.Params) || len(op.Params) > MaxLineBytes {
			return fail("checkpoint_invalid")
		}
		ids[op.ID] = true
		switch op.Method {
		case "initialize", "initialized", "thread/start", "turn/start", "turn/steer", "turn/interrupt", "$server_reply":
		default:
			return fail("checkpoint_invalid")
		}
		switch op.Phase {
		case Intent, Attempted, Written, Answered, NotSubmitted:
		default:
			return fail("checkpoint_invalid")
		}
		if op.Notification != (op.Method == "initialized" || op.Method == "$server_reply") {
			return fail("checkpoint_invalid")
		}
		if len(op.Result) > 0 && !json.Valid(op.Result) || op.RPCError != nil && len(op.RPCError.Data) > 0 && !json.Valid(op.RPCError.Data) {
			return fail("checkpoint_invalid")
		}
	}
	bytesUsed := len(s.Partial)
	seen := map[string]bool{}
	for _, ev := range s.Inbox {
		if ev.Identity == "" || seen[ev.Identity] || !json.Valid(ev.Raw) || len(ev.Raw) > limits.InboxBytes-bytesUsed {
			return fail("checkpoint_invalid")
		}
		if _, err := cursorNumber(s.Binding.Journal, ev.Cursor); err != nil {
			return fail("checkpoint_invalid")
		}
		seen[ev.Identity] = true
		bytesUsed += len(ev.Raw)
	}
	if bytesUsed > limits.InboxBytes {
		return fail("checkpoint_invalid")
	}
	seen = map[string]bool{}
	for _, request := range s.ServerRequests {
		if request.Source == "" || seen[request.Source] || request.Method == "" || !json.Valid(request.ID) || len(request.Params) > 0 && !json.Valid(request.Params) || request.InputID != 0 && (request.InputID < FirstID || request.InputID >= s.NextID || !request.Claimed) || request.Written && (!request.Claimed || request.InputID == 0) {
			return fail("checkpoint_invalid")
		}
		seen[request.Source] = true
	}
	return nil
}
