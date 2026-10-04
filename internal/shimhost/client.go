//go:build !windows

package shimhost

import (
	"context"
	"encoding/json"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hollis-labs/substrate/harness/shim"
)

// Client pins identity and journal in hello, before any controller takeover.
// Teardown checks same-uid peer credentials and a pidfd before sending hello.
// This protocol authenticates the client to the server, not the reverse.
type Client struct {
	socket      *net.UnixConn
	Epoch       string
	Journal     string
	Frames      <-chan shim.Frame
	frames      chan shim.Frame
	done        chan struct{}
	once        sync.Once
	writeMu     sync.Mutex
	counter     atomic.Uint64
	stopContext func() bool
}

func Connect(ctx context.Context, path, secret, session, instance, generation, role, journal string, takeover bool, expectedPID ...int) (*Client, error) {
	return connect(ctx, path, secret, session, instance, generation, role, journal, takeover, expectedPID, nil)
}

// dialControl is the transport boundary; refusal and absence tests inject dial failures.
var dialControl = func(ctx context.Context, path string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", path)
}

func connect(ctx context.Context, path, secret, session, instance, generation, role, journal string, takeover bool, expectedPID []int, beforeHello func(*net.UnixConn) error) (*Client, error) {
	conn, err := dialControl(ctx, path)
	if err != nil {
		return nil, err
	}
	socket, ok := conn.(*net.UnixConn)
	if !ok {
		_ = conn.Close()
		return nil, fail("unsupported", "unix control socket required")
	}
	if len(expectedPID) > 0 {
		pid, err := peerPID(socket)
		if err != nil {
			_ = socket.Close()
			return nil, err
		}
		if pid <= 0 || expectedPID[0] > 0 && pid != expectedPID[0] {
			_ = socket.Close()
			return nil, fail("identity_mismatch", "same-uid peer PID differs from recorded host")
		}
	}
	if beforeHello != nil {
		if err := beforeHello(socket); err != nil {
			_ = socket.Close()
			return nil, err
		}
	}
	success := false
	defer func() {
		if !success {
			_ = socket.Close()
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err = socket.SetDeadline(deadline); err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = socket.Close() })
	defer func() {
		if !success {
			stop()
		}
	}()
	challenge, err := shim.ReadFrame(socket)
	if err != nil {
		return nil, err
	}
	var body struct {
		Nonce string `json:"nonce"`
		Epoch string `json:"controller_epoch"`
	}
	if challenge.Type != "hello" || challenge.Session != session || json.Unmarshal(challenge.Body, &body) != nil || body.Nonce == "" {
		return nil, fail("identity_mismatch", "invalid server challenge")
	}
	hello := shim.Hello{Major: shim.ProtocolMajor, Minor: shim.ProtocolMinor, Role: role, Proof: shim.Proof(secret, body.Nonce, session, role), Instance: instance, Generation: generation, Epoch: body.Epoch, Journal: journal, Takeover: takeover}
	raw, err := json.Marshal(hello)
	if err != nil {
		return nil, err
	}
	if err = shim.WriteFrame(socket, shim.Frame{Major: shim.ProtocolMajor, Type: "hello", RequestID: "hello", Session: session, Body: raw}); err != nil {
		return nil, err
	}
	response, err := shim.ReadFrame(socket)
	if err != nil {
		return nil, err
	}
	if response.Type == "error" {
		var refusal struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(response.Body, &refusal) != nil || refusal.Code == "" {
			return nil, fail("invalid_frame", "invalid hello refusal")
		}
		return nil, fail(refusal.Code, "shim hello refused")
	}
	var ready struct {
		Epoch   string `json:"controller_epoch"`
		Journal string `json:"journal"`
	}
	if response.Type != "hello" || response.Session != session || json.Unmarshal(response.Body, &ready) != nil || ready.Journal == "" {
		return nil, fail("invalid_frame", "invalid hello response")
	}
	if journal != "" && ready.Journal != journal {
		return nil, fail("identity_mismatch", "host returned another journal")
	}
	if err = socket.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	c := &Client{socket: socket, Epoch: ready.Epoch, Journal: ready.Journal, frames: make(chan shim.Frame, 256), done: make(chan struct{}), stopContext: stop}
	c.Frames = c.frames
	go c.read(session)
	success = true
	return c, nil
}
func (c *Client) read(session string) {
	defer close(c.frames)
	defer func() { _ = c.Close() }()
	for {
		frame, err := shim.ReadFrame(c.socket)
		if err != nil {
			return
		}
		if frame.Type == "health" {
			if c.Send(session, "health", map[string]bool{"pong": true}) != nil {
				return
			}
			continue
		}
		select {
		case c.frames <- frame:
		case <-c.done:
			return
		}
	}
}
func (c *Client) Send(session, kind string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return c.SendFrame(shim.Frame{Major: shim.ProtocolMajor, Type: kind, RequestID: "request-" + strconv.FormatUint(c.counter.Add(1), 10), Session: session, Epoch: c.Epoch, Body: raw})
}
func (c *Client) SendFrame(frame shim.Frame) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.socket.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	return shim.WriteFrame(c.socket, frame)
}
func (c *Client) Replay(session, cursor string) error {
	return c.Send(session, "replay", map[string]string{"after_cursor": cursor})
}
func (c *Client) Ack(session, cursor string) error {
	return c.Send(session, "ack", map[string]string{"cursor": cursor})
}
func (c *Client) Close() error {
	var err error
	c.once.Do(func() { c.stopContext(); close(c.done); err = c.socket.Close() })
	return err
}
