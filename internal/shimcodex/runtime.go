//go:build !windows

package shimcodex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/hollis-labs/agentkit/agentsessions"
	"github.com/hollis-labs/substrate/harness/shim"
	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/tether/internal/shimhost"
)

type Config struct {
	ID              string
	Receipt         shimhost.Receipt
	Store           Store
	Fresh           bool
	Limits          Limits
	Validate        func(context.Context) error
	OnSession       func(*Session)
	OnDetach        func(error)
	DeliveryChecker DeliveryChecker
	Deliver         DeliveryHandler
	RecoverInbox    InboxWireReader
}
type Runtime struct{ Config Config }

func (r *Runtime) ID() string { return r.Config.ID }
func (*Runtime) Kind() string { return "cli" }
func (*Runtime) Caps() agentsessions.Capabilities {
	return agentsessions.Capabilities{JsonRpcStdio: true, ProviderSessionID: true, BinaryRequired: true}
}
func (r *Runtime) Prepare(ctx context.Context) error {
	if r.Config.ID == "" || r.Config.Store == nil || r.Config.Validate == nil {
		return fail("invalid_config")
	}
	return r.Config.Validate(ctx)
}
func (r *Runtime) Start(ctx context.Context, opts agentsessions.StartOptions) (agentsessions.Session, error) {
	if err := r.Prepare(ctx); err != nil {
		return nil, err
	}
	receipt := r.Config.Receipt
	if receipt.HostPID <= 0 || receipt.ProviderPID <= 0 || receipt.Journal == "" {
		return nil, fail("identity_mismatch")
	}
	lock, err := shimhost.Lock(filepath.Join(filepath.Dir(receipt.DescriptorPath), "codex-controller.lock"))
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			_ = lock.Close()
		}
	}()
	spec, err := shimhost.Descriptor(receipt)
	if err != nil {
		return nil, err
	}
	connectionCtx, disconnect := context.WithCancel(context.Background())
	stopStartup := context.AfterFunc(ctx, disconnect)
	c, err := shimhost.Connect(connectionCtx, receipt.SocketPath, spec.Secret, spec.Session, spec.Instance, strconv.FormatUint(spec.Generation, 10), "controller", receipt.Journal, !r.Config.Fresh, receipt.HostPID)
	stopStartup()
	if err != nil {
		disconnect()
		return nil, err
	}
	defer func() {
		if !success {
			disconnect()
			_ = c.Close()
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	epoch, err := strconv.ParseUint(c.Epoch, 10, 64)
	if err != nil || epoch == 0 {
		return nil, fail("epoch_mismatch")
	}
	engine, err := Open(ctx, r.Config.Store, Binding{Session: spec.Session, Instance: spec.Instance, Generation: spec.Generation, Operation: receipt.OperationKey, Journal: c.Journal, Attempt: receipt.SubmissionAttemptID, Fingerprint: receipt.Fingerprint}, epoch, r.Config.Fresh, r.Config.Limits)
	if err != nil {
		return nil, err
	}
	if err := engine.RestoreInboxWire(ctx, r.Config.RecoverInbox); err != nil {
		return nil, err
	}
	t := &hostTransport{client: c, spec: spec, validate: r.Config.Validate, pending: make(map[string]chan shim.Frame), lock: lock, disconnect: disconnect}
	s, err := NewSession(engine, t, receipt.ProviderPID)
	if err != nil {
		return nil, err
	}
	s.onSessionID = opts.OnSessionID
	s.deliveryChecker = r.Config.DeliveryChecker
	s.deliver = r.Config.Deliver
	defer func() {
		if !success {
			s.cancel()
		}
	}()
	if !r.Config.Fresh && s.deliver != nil && len(engine.Snapshot().Inbox) != 0 {
		// A retained batch can already occupy the full inbox allowance. Settle
		// that exact frozen batch before admitting replay/attach records; racing
		// the reader against delivery can otherwise refuse the next frame and
		// cancel delivery. Unsupported obligations remain private and refuse
		// this controller without ACKing or changing the provider process.
		work, stop := bounded(ctx)
		err = s.deliverOnce(work)
		stop()
		if err != nil {
			return nil, err
		}
	}
	if err = t.Replay(ctx, engine.Snapshot().Cursor); err != nil {
		return nil, err
	}
	success = true
	s.workers.Add(2)
	go func() { defer s.workers.Done(); t.read(s, r.Config.OnDetach) }()
	go func() { defer s.workers.Done(); s.serverRequests(opts.JsonRpcRequestHook) }()
	if s.deliver != nil {
		s.workers.Add(1)
		go func() { defer s.workers.Done(); s.deliverOutputs() }()
	}
	if r.Config.OnSession != nil {
		r.Config.OnSession(s)
	}
	return s, nil
}

type hostTransport struct {
	client     *shimhost.Client
	spec       shim.Launch
	validate   func(context.Context) error
	mu         sync.Mutex
	pending    map[string]chan shim.Frame
	once       sync.Once
	lock       *os.File
	disconnect context.CancelFunc
}

func (t *hostTransport) Validate(ctx context.Context) error {
	if err := t.validate(ctx); err != nil {
		return err
	}
	return ctx.Err()
}

func (t *hostTransport) Inject(ctx context.Context, key string, data []byte) error {
	if len(data) > shim.OutputChunk {
		return fail("frame_too_large")
	}
	if err := t.validate(ctx); err != nil {
		return err
	}
	ch := make(chan shim.Frame, 1)
	t.mu.Lock()
	if _, ok := t.pending[key]; ok {
		t.mu.Unlock()
		return fail("operation_conflict")
	}
	t.pending[key] = ch
	t.mu.Unlock()
	defer func() { t.mu.Lock(); delete(t.pending, key); t.mu.Unlock() }()
	raw, err := json.Marshal(shim.Inject{Key: key, Actor: t.spec.Actor, Subject: t.spec.Subject, Mode: "input", Delivery: "immediate", Generation: strconv.FormatUint(t.spec.Generation, 10), Data: base64.StdEncoding.EncodeToString(data)})
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err = t.client.SendFrame(shim.Frame{Major: shim.ProtocolMajor, Type: "inject", RequestID: key, Session: t.spec.Session, Epoch: t.client.Epoch, Body: raw}); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case frame := <-ch:
		var receipt struct {
			Code string `json:"code"`
		}
		if frame.Type != "result" || json.Unmarshal(frame.Body, &receipt) != nil || receipt.Code != "bytes_written" {
			return fail("outcome_unknown")
		}
		return nil
	}
}
func (t *hostTransport) Replay(ctx context.Context, cursor string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return t.client.Replay(t.spec.Session, cursor)
}
func (t *hostTransport) Ack(ctx context.Context, cursor string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return t.client.Ack(t.spec.Session, cursor)
}
func (t *hostTransport) Close() error {
	var err error
	t.once.Do(func() { t.disconnect(); err = t.client.Close(); _ = t.lock.Close() })
	return err
}

func (t *hostTransport) read(s *Session, onDetach func(error)) {
	code, terminalErr := 0, fail("detached")
	defer func() {
		if onDetach != nil {
			onDetach(terminalErr)
		}
		s.Finish(code, terminalErr)
	}()
	for {
		select {
		case <-s.ctx.Done():
			return
		case frame, ok := <-t.client.Frames:
			if !ok {
				return
			}
			if frame.Session != t.spec.Session {
				terminalErr = fail("identity_mismatch")
				return
			}
			if strings.HasPrefix(frame.ReplyTo, "codex-") {
				t.mu.Lock()
				ch := t.pending[frame.ReplyTo]
				t.mu.Unlock()
				if ch != nil {
					select {
					case ch <- frame:
					default:
					}
				}
				continue
			}
			if frame.Type == "error" {
				terminalErr = fail("host_refused")
				return
			}
			if frame.Type == "result" {
				var result struct {
					High string `json:"high_water"`
				}
				if json.Unmarshal(frame.Body, &result) == nil && result.High != "" {
					s.setHighWater(result.High)
				}
				continue
			}
			if frame.Type != "event" {
				continue
			}
			var envelope struct {
				Event mesh.Event `json:"event"`
			}
			if json.Unmarshal(frame.Body, &envelope) != nil {
				terminalErr = fail("protocol_invalid")
				return
			}
			ev := envelope.Event
			work, stop := bounded(s.ctx)
			if ev.Kind == "shim.output_gap" {
				stop()
				terminalErr = fail("output_gap")
				return
			}
			if ev.Kind == "shim.output" {
				var output struct {
					Stream   string `json:"stream"`
					Encoding string `json:"encoding"`
					Data     string `json:"data"`
				}
				if json.Unmarshal(ev.Payload, &output) != nil || output.Encoding != "base64" {
					stop()
					terminalErr = fail("protocol_invalid")
					return
				}
				data, err := base64.StdEncoding.DecodeString(output.Data)
				if err == nil {
					err = s.AcceptOutput(work, ev.Cursor, output.Stream, data)
				}
				stop()
				if err != nil {
					terminalErr = err
					return
				}
				if s.onSessionID != nil && s.ProviderSessionID() != "" {
					s.onSessionID(s.ProviderSessionID())
				}
				continue
			}
			if ev.Kind == "shim.exit" {
				var exit shim.Exit
				if json.Unmarshal(ev.Payload, &exit) != nil {
					stop()
					terminalErr = fail("protocol_invalid")
					return
				}
				if err := s.engine.AcceptExit(work, ev.Cursor, exit); err != nil {
					stop()
					terminalErr = err
					return
				}
				// Finish cancels controller workers. Settle the authenticated exit
				// obligation before that cancellation; failure retains the inbox.
				_ = s.deliverOnce(work)
				if err := t.Ack(work, s.engine.Snapshot().Cursor); err != nil {
					stop()
					terminalErr = err
					return
				}
				stop()
				code = exit.Status
				if exit.Signal != 0 {
					code = 128 + exit.Signal
				}
				if len(s.engine.Snapshot().Partial) != 0 {
					terminalErr = fail("protocol_truncated")
					return
				}
				terminalErr = nil
				return
			}
			err := s.engine.AcceptMetadata(work, s.engine.Snapshot().Epoch, ev.Cursor, ev.Kind, ev.Payload)
			if err == nil {
				err = t.Ack(work, s.engine.Snapshot().Cursor)
			}
			stop()
			if err != nil {
				terminalErr = err
				return
			}
			s.signal() // Metadata never implies provider completion.
		}
	}
}

var _ agentsessions.Runtime = (*Runtime)(nil)
