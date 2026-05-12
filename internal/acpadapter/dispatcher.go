package acpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// HandlerFunc handles an inbound JSON-RPC request. Returns either a
// result value (will be JSON-encoded) or an *RPCError to send back to
// the client. Returning a non-RPCError error is treated as
// ErrCodeInternal with the error's message.
type HandlerFunc func(ctx context.Context, params json.RawMessage) (any, error)

// NotificationFunc handles an inbound JSON-RPC notification. No
// response is sent regardless of return value; errors are logged
// (eventually) and otherwise discarded per JSON-RPC 2.0 semantics.
type NotificationFunc func(ctx context.Context, params json.RawMessage)

// Dispatcher is the bidirectional JSON-RPC engine. It owns:
//
//   - A reader goroutine that pulls inbound messages off the Reader
//     and routes requests/notifications to registered handlers and
//     responses to pending outbound waiters.
//   - The Writer that serializes outbound messages.
//   - The pending-response map that correlates outbound requests with
//     their responses.
//
// Dispatcher is safe for concurrent use by multiple goroutines: handler
// invocations may originate Call() against the same dispatcher to send
// outbound requests (e.g. fs/read_text_file in future), and the writer
// is internally serialized.
type Dispatcher struct {
	r *Reader
	w *Writer

	methodHandlers       map[string]HandlerFunc
	notificationHandlers map[string]NotificationFunc

	// inFlight tracks goroutines spawned for inbound request and
	// notification handlers so Run() can drain them before returning
	// on EOF. Without this drain, tests (and any caller using a
	// finite input source) race against the handler write.
	inFlight sync.WaitGroup

	// outbound request correlation: outbound id → response chan.
	// Outbound IDs use a separate counter from inbound to avoid
	// collisions. They use negative integers (encoded as JSON numbers)
	// so wire-level discrimination is obvious for debugging.
	pendingMu sync.Mutex
	pending   map[int64]chan *Message
	nextOutID atomic.Int64 // counter; allocateOutID negates it on emit
}

// NewDispatcher constructs a Dispatcher around the given reader/writer.
// Methods + notifications must be registered before Run() is called.
func NewDispatcher(r *Reader, w *Writer) *Dispatcher {
	return &Dispatcher{
		r:                    r,
		w:                    w,
		methodHandlers:       map[string]HandlerFunc{},
		notificationHandlers: map[string]NotificationFunc{},
		pending:              map[int64]chan *Message{},
	}
}

// HandleMethod registers a handler for inbound requests with the given
// method name. Subsequent registrations overwrite earlier ones.
func (d *Dispatcher) HandleMethod(name string, h HandlerFunc) {
	d.methodHandlers[name] = h
}

// HandleNotification registers a handler for inbound notifications.
func (d *Dispatcher) HandleNotification(name string, h NotificationFunc) {
	d.notificationHandlers[name] = h
}

// Run drives the dispatcher's read loop until ctx is canceled or the
// reader returns io.EOF (peer closed stdin). Handlers run in their
// own goroutines so a slow handler doesn't block subsequent inbound
// messages — request IDs serialize ordering already.
func (d *Dispatcher) Run(ctx context.Context) error {
	for {
		// Drain pending responses if context is canceled, so any
		// goroutine blocked on a Call() unblocks with a context error.
		select {
		case <-ctx.Done():
			d.cancelPending(ctx.Err())
			return ctx.Err()
		default:
		}

		msg, err := d.r.Read()
		if err != nil {
			// EOF or transport error: drain in-flight handlers so
			// their responses make it onto the wire before we exit
			// (otherwise finite-input callers like tests race the
			// goroutine), then terminate.
			d.inFlight.Wait()
			d.cancelPending(err)
			return err
		}

		switch {
		case msg.IsRequest():
			d.inFlight.Add(1)
			go func() {
				defer d.inFlight.Done()
				d.dispatchRequest(ctx, msg)
			}()
		case msg.IsNotification():
			d.inFlight.Add(1)
			go func() {
				defer d.inFlight.Done()
				d.dispatchNotification(ctx, msg)
			}()
		case msg.IsResponse():
			d.dispatchResponse(msg)
		default:
			// Spec violation; respond with InvalidRequest if there's
			// an id to attach the error to, otherwise drop.
			if len(msg.ID) > 0 {
				_ = d.writeError(msg.ID, ErrCodeInvalidRequest, "request must include method or response must include id")
			}
		}
	}
}

func (d *Dispatcher) dispatchRequest(ctx context.Context, msg *Message) {
	h, ok := d.methodHandlers[msg.Method]
	if !ok {
		_ = d.writeError(msg.ID, ErrCodeMethodNotFound, fmt.Sprintf("unknown method: %s", msg.Method))
		return
	}
	result, err := h(ctx, msg.Params)
	if err != nil {
		var rpcErr *RPCError
		if errors.As(err, &rpcErr) {
			_ = d.writeError(msg.ID, rpcErr.Code, rpcErr.Message)
			return
		}
		_ = d.writeError(msg.ID, ErrCodeInternal, err.Error())
		return
	}
	_ = d.writeResult(msg.ID, result)
}

func (d *Dispatcher) dispatchNotification(ctx context.Context, msg *Message) {
	h, ok := d.notificationHandlers[msg.Method]
	if !ok {
		// Per spec: implementations SHOULD ignore unrecognized notifications.
		return
	}
	h(ctx, msg.Params)
}

func (d *Dispatcher) dispatchResponse(msg *Message) {
	id, err := decodeOutboundID(msg.ID)
	if err != nil {
		// Response ID we can't parse — orphan, drop.
		return
	}
	d.pendingMu.Lock()
	ch, ok := d.pending[id]
	if ok {
		delete(d.pending, id)
	}
	d.pendingMu.Unlock()
	if !ok {
		return
	}
	// Non-blocking send; channel is buffered.
	select {
	case ch <- msg:
	default:
	}
}

func (d *Dispatcher) cancelPending(err error) {
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()
	for id, ch := range d.pending {
		// Synthesize an error response so blocked Call() callers exit.
		ch <- &Message{Error: &RPCError{Code: ErrCodeInternal, Message: err.Error()}}
		delete(d.pending, id)
	}
}

// Notify emits an outbound notification. No response is awaited.
func (d *Dispatcher) Notify(method string, params any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}
	return d.w.Write(&Message{
		Method: method,
		Params: raw,
	})
}

// Call emits an outbound request and blocks until the response arrives
// or ctx is canceled. Returns the response Result raw bytes; the caller
// decodes into the expected type. Used by future fs/terminal/permission
// proxying — no MVP code path triggers it.
func (d *Dispatcher) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	raw, err := marshalParams(params)
	if err != nil {
		return nil, err
	}
	id := d.allocateOutID()
	idJSON, _ := json.Marshal(id)

	respCh := make(chan *Message, 1)
	d.pendingMu.Lock()
	d.pending[id] = respCh
	d.pendingMu.Unlock()

	if err := d.w.Write(&Message{
		ID:     idJSON,
		Method: method,
		Params: raw,
	}); err != nil {
		d.pendingMu.Lock()
		delete(d.pending, id)
		d.pendingMu.Unlock()
		return nil, err
	}

	select {
	case <-ctx.Done():
		d.pendingMu.Lock()
		delete(d.pending, id)
		d.pendingMu.Unlock()
		return nil, ctx.Err()
	case resp := <-respCh:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	}
}

// allocateOutID returns the next outbound request ID. We use negative
// integers so they're trivially distinguishable from inbound IDs (which
// editors typically allocate as positive integers starting from 0).
func (d *Dispatcher) allocateOutID() int64 {
	n := d.nextOutID.Add(1)
	return -n
}

// decodeOutboundID parses a JSON-RPC id field as the int64 we encoded
// in allocateOutID. Returns an error for non-numeric or non-negative
// ids (which means the response isn't ours to wait for).
func decodeOutboundID(raw json.RawMessage) (int64, error) {
	var id int64
	if err := json.Unmarshal(raw, &id); err != nil {
		return 0, err
	}
	if id >= 0 {
		return 0, errors.New("not an outbound id")
	}
	return id, nil
}

// writeResult emits a successful response to id with result.
func (d *Dispatcher) writeResult(id json.RawMessage, result any) error {
	raw, err := marshalResult(result)
	if err != nil {
		return d.writeError(id, ErrCodeInternal, fmt.Sprintf("marshal result: %v", err))
	}
	return d.w.Write(&Message{ID: id, Result: raw})
}

// writeError emits an error response to id.
func (d *Dispatcher) writeError(id json.RawMessage, code int, message string) error {
	return d.w.Write(&Message{ID: id, Error: &RPCError{Code: code, Message: message}})
}

// marshalParams encodes a params value to json.RawMessage. nil → null.
func marshalParams(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}

// marshalResult encodes a result value. nil → "null" (so the wire
// envelope still has a result field, distinguishing success-with-null
// from missing result).
func marshalResult(v any) (json.RawMessage, error) {
	if v == nil {
		return json.RawMessage("null"), nil
	}
	return json.Marshal(v)
}
