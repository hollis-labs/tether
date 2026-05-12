package acpadapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

func TestReader_RoundTripRequest(t *testing.T) {
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}` + "\n")
	r := NewReader(in)
	msg, err := r.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !msg.IsRequest() {
		t.Fatalf("expected request, got method=%q id=%s", msg.Method, msg.ID)
	}
	if msg.Method != "initialize" {
		t.Errorf("Method = %q, want initialize", msg.Method)
	}
}

func TestReader_RoundTripNotification(t *testing.T) {
	in := strings.NewReader(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"s1"}}` + "\n")
	r := NewReader(in)
	msg, err := r.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !msg.IsNotification() {
		t.Fatalf("expected notification, got method=%q id=%s", msg.Method, msg.ID)
	}
}

func TestReader_RoundTripResponse(t *testing.T) {
	in := strings.NewReader(`{"jsonrpc":"2.0","id":-1,"result":null}` + "\n")
	r := NewReader(in)
	msg, err := r.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !msg.IsResponse() {
		t.Fatalf("expected response, got method=%q id=%s", msg.Method, msg.ID)
	}
}

func TestReader_SkipsBlankLines(t *testing.T) {
	in := strings.NewReader("\n\n" + `{"jsonrpc":"2.0","id":1,"method":"x"}` + "\n")
	r := NewReader(in)
	msg, err := r.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if msg.Method != "x" {
		t.Errorf("Method = %q, want x", msg.Method)
	}
}

func TestReader_EOF(t *testing.T) {
	in := strings.NewReader("")
	r := NewReader(in)
	_, err := r.Read()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("expected io.EOF, got %v", err)
	}
}

func TestReader_RejectsWrongVersion(t *testing.T) {
	in := strings.NewReader(`{"jsonrpc":"1.0","id":1,"method":"x"}` + "\n")
	r := NewReader(in)
	_, err := r.Read()
	if err == nil {
		t.Fatal("expected error for wrong jsonrpc version")
	}
	if !strings.Contains(err.Error(), "1.0") {
		t.Errorf("error %q should mention the bad version", err.Error())
	}
}

func TestWriter_RoundTrip(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	id, _ := json.Marshal(1)
	if err := w.Write(&Message{ID: id, Method: "initialize"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out := buf.String()
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("output missing trailing newline: %q", out)
	}
	if !strings.Contains(out, `"jsonrpc":"2.0"`) {
		t.Errorf("output missing jsonrpc version: %q", out)
	}
	if !strings.Contains(out, `"method":"initialize"`) {
		t.Errorf("output missing method: %q", out)
	}
	// Roundtrip back through Reader.
	r := NewReader(strings.NewReader(out))
	msg, err := r.Read()
	if err != nil {
		t.Fatalf("Reader.Read: %v", err)
	}
	if msg.Method != "initialize" {
		t.Errorf("roundtripped method = %q, want initialize", msg.Method)
	}
}

func TestWriter_ConcurrentSafe(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	var wg sync.WaitGroup
	const N = 50
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id, _ := json.Marshal(i)
			_ = w.Write(&Message{ID: id, Method: "x"})
		}(i)
	}
	wg.Wait()
	// Each Write produces one line; verify exactly N lines and each
	// line is a parseable Message (no interleaved bytes).
	r := NewReader(&buf)
	count := 0
	for {
		msg, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read message %d: %v", count, err)
		}
		if msg.Method != "x" {
			t.Errorf("message %d method = %q, want x", count, msg.Method)
		}
		count++
	}
	if count != N {
		t.Errorf("got %d messages, want %d", count, N)
	}
}
