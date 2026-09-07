package a2aadapter

// executor.go — TetherExecutor bridges the a2a-go SDK's AgentExecutor
// extension point into Tether's canonical, delivery-backed messaging
// service. See doc.go for the full design rationale (ID discipline,
// message-vs-task decision, unsupported-operation signaling).

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	messaging "github.com/hollis-labs/go-messaging"
)

// MessageSender is the narrow seam TetherExecutor depends on for relaying
// an inbound A2A message into Tether's canonical messaging service.
// *store.Store's InboxStore (via MessagingStore()) satisfies this
// directly — the SAME Send path every other Tether message goes through,
// so a relayed A2A message gets a real delivery obligation, is traceable
// via T09's GET /messages/{id}/trace, and can be redriven if delivery
// fails. This package adds no separate retry/idempotency layer of its
// own (T10 acceptance #3).
type MessageSender interface {
	Send(ctx context.Context, env messaging.Envelope) (messaging.Envelope, error)
}

// TetherExecutor implements a2asrv.AgentExecutor for one AgentBinding.
type TetherExecutor struct {
	binding     AgentBinding
	target      messaging.Address
	sender      MessageSender
	coordinator *taskCoordinator
}

var _ a2asrv.AgentExecutor = (*TetherExecutor)(nil)

// peerAddress represents the external A2A caller as a Tether
// messaging.KindService identity — deliberately not KindAgent or
// KindSession, so an external A2A peer can never be confused with a
// Tether-native registered agent or a live session (T10 acceptance #2).
func (e *TetherExecutor) peerAddress() messaging.Address {
	return messaging.Address{Kind: messaging.KindService, Authority: "a2a", ID: e.binding.ID}
}

// a2aRelayPayload is the JSON body of every Tether envelope this executor
// sends — deliberately minimal (just the extracted text), since the
// authoritative record of the original A2A message shape lives with the
// A2A peer; Tether's copy exists to notify a consumer and correlate,
// not to be a full protocol mirror.
type a2aRelayPayload struct {
	Text string `json:"text"`
}

func extractText(msg *a2a.Message) string {
	var texts []string
	for _, p := range msg.Parts {
		if t := p.Text(); t != "" {
			texts = append(texts, t)
		}
	}
	return strings.Join(texts, "\n")
}

// relayMetadata builds the envelope Metadata carrying A2A correlation IDs
// — the ONLY place those IDs appear on the Tether side (T10 acceptance
// #2: "A2A context/task IDs never replace AgentID/SESSION").
func relayMetadata(msg *a2a.Message, taskID a2a.TaskID) map[string]string {
	md := map[string]string{"a2a_message_id": msg.ID}
	if msg.ContextID != "" {
		md["a2a_context_id"] = msg.ContextID
	}
	if taskID != "" {
		md["a2a_task_id"] = string(taskID)
	}
	return md
}

// Execute implements a2asrv.AgentExecutor. See doc.go for the message-only
// vs. delegated-work decision (AgentBinding.TaskMode) and why a
// delegated task's completion is never decided here.
func (e *TetherExecutor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		msg := execCtx.Message
		if msg == nil {
			yield(nil, a2a.NewError(a2a.ErrInvalidParams, "message is required"))
			return
		}

		payload, err := json.Marshal(a2aRelayPayload{Text: extractText(msg)})
		if err != nil {
			yield(nil, a2a.NewError(a2a.ErrInternalError, "failed to encode relayed message"))
			return
		}

		if !e.binding.TaskMode {
			e.executeMessageOnly(ctx, execCtx, msg, payload, yield)
			return
		}
		e.executeDelegatedTask(ctx, execCtx, msg, payload, yield)
	}
}

func (e *TetherExecutor) executeMessageOnly(
	ctx context.Context, execCtx *a2asrv.ExecutorContext, msg *a2a.Message, payload json.RawMessage,
	yield func(a2a.Event, error) bool,
) {
	env := messaging.Envelope{
		Kind:     messaging.MsgKindNotice,
		From:     e.peerAddress(),
		To:       e.target,
		Payload:  payload,
		Metadata: relayMetadata(msg, ""),
	}
	if _, err := e.sender.Send(ctx, env); err != nil {
		yield(nil, a2a.NewError(a2a.ErrInternalError, fmt.Sprintf("failed to relay message: %v", err)))
		return
	}

	reply := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("relayed"))
	reply.ContextID = execCtx.ContextID
	yield(reply, nil)
}

func (e *TetherExecutor) executeDelegatedTask(
	ctx context.Context, execCtx *a2asrv.ExecutorContext, msg *a2a.Message, payload json.RawMessage,
	yield func(a2a.Event, error) bool,
) {
	task := a2a.NewSubmittedTask(execCtx, msg)
	e.coordinator.register(execCtx.TaskID, e.binding.ID)
	if !yield(task, nil) {
		return
	}

	env := messaging.Envelope{
		Kind:     messaging.MsgKindRequest,
		From:     e.peerAddress(),
		To:       e.target,
		ThreadID: string(execCtx.TaskID),
		Payload:  payload,
		Metadata: relayMetadata(msg, execCtx.TaskID),
	}
	if _, err := e.sender.Send(ctx, env); err != nil {
		// A Task event was already yielded above -- per AgentExecutor's
		// own doc comment, a failure past that point is reported as a
		// TaskStatusUpdateEvent in a failed state, not a returned error.
		failMsg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(fmt.Sprintf("failed to relay delegated work: %v", err)))
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed, failMsg), nil)
		return
	}

	if !yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateWorking, nil), nil) {
		return
	}

	timeout := time.NewTimer(e.binding.awaitTimeout())
	defer timeout.Stop()
	outcome, ok := e.coordinator.await(ctx, execCtx.TaskID, timeout.C)
	if !ok {
		waitMsg := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(
			"no consumer transition was received within the wait window; call the transition endpoint to resume"))
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateInputRequired, waitMsg), nil)
		return
	}
	yield(a2a.NewStatusUpdateEvent(execCtx, outcome.state, outcome.message), nil)
}

// Cancel implements a2asrv.AgentExecutor. Unblocks a currently-waiting
// delegated-task Execute call (if any) with a Canceled outcome, and
// separately reports the cancelation on Cancel's own event stream,
// matching the pattern AgentExecutor's own doc comment describes (the
// task store reconciles a concurrent Execute/Cancel status update via
// optimistic concurrency control).
func (e *TetherExecutor) Cancel(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		e.coordinator.resolve(execCtx.TaskID, e.binding.ID, taskOutcome{state: a2a.TaskStateCanceled})
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
	}
}
