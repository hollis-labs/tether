package acpadapter

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// jsonRPCVersion is the JSON-RPC version string ACP uses.
const jsonRPCVersion = "2.0"

// Message is the union envelope for all JSON-RPC 2.0 message variants
// (request, response, notification). Optional fields use json.RawMessage
// + omitempty so a single struct can both be received and sent without
// needing per-direction types.
//
// Discrimination at receive time:
//   - Method != "" && len(ID) > 0  → request
//   - Method != "" && len(ID) == 0 → notification
//   - Method == "" && len(ID) > 0  → response (Result XOR Error)
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// IsRequest reports whether m is a method invocation expecting a response.
func (m *Message) IsRequest() bool { return m.Method != "" && len(m.ID) > 0 }

// IsNotification reports whether m is a one-way method call.
func (m *Message) IsNotification() bool { return m.Method != "" && len(m.ID) == 0 }

// IsResponse reports whether m is a response to a prior request.
func (m *Message) IsResponse() bool { return m.Method == "" && len(m.ID) > 0 }

// RPCError is the JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return "<nil rpc error>"
	}
	return fmt.Sprintf("acp rpc error %d: %s", e.Code, e.Message)
}

// Standard JSON-RPC 2.0 error codes (subset relevant to ACP).
const (
	ErrCodeParseError     = -32700
	ErrCodeInvalidRequest = -32600
	ErrCodeMethodNotFound = -32601
	ErrCodeInvalidParams  = -32602
	ErrCodeInternal       = -32603
)

// Reader reads ACP JSON-RPC messages from r, one per line, ignoring
// blank lines. Not safe for concurrent use; one goroutine reads.
//
// The bufio.Scanner default 64KB line limit is bumped to 4MB so large
// content blocks (resource_link with embedded data once we support it,
// or long agent message chunks if any client batches) don't truncate.
type Reader struct {
	scanner *bufio.Scanner
}

// NewReader wraps r in a Reader. r is typically os.Stdin.
func NewReader(r io.Reader) *Reader {
	s := bufio.NewScanner(r)
	const maxLine = 4 << 20 // 4MB; spec doesn't define a limit, but oversize messages are pathological
	s.Buffer(make([]byte, 0, 64*1024), maxLine)
	return &Reader{scanner: s}
}

// Read returns the next message. Returns io.EOF when the underlying
// reader closes. Returns a parse error (with the offending line in
// the wrapped error) for malformed JSON; callers may choose to send
// a JSON-RPC parse-error response and continue.
func (r *Reader) Read() (*Message, error) {
	for {
		if !r.scanner.Scan() {
			if err := r.scanner.Err(); err != nil {
				return nil, err
			}
			return nil, io.EOF
		}
		line := r.scanner.Bytes()
		// Skip blank lines defensively. Spec doesn't allow them but some
		// clients add trailing newlines for readability.
		if len(trimSpace(line)) == 0 {
			continue
		}
		var m Message
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, fmt.Errorf("acp: parse json-rpc envelope: %w (line=%q)", err, string(line))
		}
		if m.JSONRPC != jsonRPCVersion {
			return nil, fmt.Errorf("acp: unexpected jsonrpc version %q (want %q)", m.JSONRPC, jsonRPCVersion)
		}
		return &m, nil
	}
}

// Writer serializes ACP JSON-RPC messages to w one per line. Safe for
// concurrent use — calls are serialized via mu so the inbound dispatch
// goroutines and outbound notification emitters don't interleave bytes
// on the same stdout.
type Writer struct {
	w  io.Writer
	mu sync.Mutex
}

// NewWriter wraps w. w is typically os.Stdout.
func NewWriter(w io.Writer) *Writer {
	return &Writer{w: w}
}

// Write encodes m as a single newline-terminated JSON object. Sets
// JSONRPC to "2.0" if unset so callers don't have to.
func (w *Writer) Write(m *Message) error {
	if m.JSONRPC == "" {
		m.JSONRPC = jsonRPCVersion
	}
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("acp: marshal message: %w", err)
	}
	if containsNewline(b) {
		// Spec: "Messages MUST NOT contain embedded newlines." If a
		// payload field happens to contain a literal \n the json
		// encoder will produce \\n already; this guard catches the
		// bug where a Message field bypassed the encoder.
		return errors.New("acp: marshaled message contains embedded newline")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.w.Write(b); err != nil {
		return err
	}
	if _, err := w.w.Write([]byte{'\n'}); err != nil {
		return err
	}
	return nil
}

// trimSpace is a tiny ascii whitespace trim that doesn't allocate.
func trimSpace(b []byte) []byte {
	start := 0
	for start < len(b) && isSpace(b[start]) {
		start++
	}
	end := len(b)
	for end > start && isSpace(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isSpace(c byte) bool {
	switch c {
	case ' ', '\t', '\r', '\n':
		return true
	}
	return false
}

// containsNewline reports whether b has a literal \n byte. Used to
// enforce the spec's "no embedded newlines" rule.
func containsNewline(b []byte) bool {
	for _, c := range b {
		if c == '\n' {
			return true
		}
	}
	return false
}
