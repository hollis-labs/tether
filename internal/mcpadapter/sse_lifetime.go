package mcpadapter

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"time"
)

// sseLifetimeTransport bounds connection setup through the first SSE event by
// the handshake context. The established body belongs to the SDK session, which
// closes it on failed initialization, disconnect and owner shutdown.
type sseLifetimeTransport struct{ base http.RoundTripper }

func (t *sseLifetimeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet {
		return t.base.RoundTrip(req)
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(req.Context()))
	stop := context.AfterFunc(req.Context(), cancel)
	response, err := t.base.RoundTrip(req.Clone(ctx))
	if err != nil {
		stop()
		cancel()
		return nil, err
	}
	if req.Context().Err() != nil {
		stop()
		_ = response.Body.Close()
		cancel()
		return nil, req.Context().Err()
	}
	response.Body = &sseLifetimeBody{ReadCloser: response.Body, cancel: cancel, stop: stop}
	return response, nil
}

// Match go-mcp/compat's partial-event cap; the observer only buffers until
// the endpoint event. Keepalive events dropped by compat cannot transfer
// cancellation ownership before the SDK sees the endpoint.
const sseHandshakeEventLimit = 1024 * 1024

type sseLifetimeBody struct {
	io.ReadCloser
	cancel      context.CancelFunc
	stop        func() bool
	partial     []byte
	established bool
}

func (b *sseLifetimeBody) Close() error { b.stop(); b.cancel(); return b.ReadCloser.Close() }
func (b *sseLifetimeBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if !b.established {
		if observeErr := b.observeEndpoint(p[:n]); observeErr != nil {
			err = observeErr
		}
	}
	if err != nil {
		b.stop()
		b.cancel()
	}
	return n, err
}

// Observe complete LF/CRLF blocks, matching go-mcp/compat's event selection.
// Comments, filtered keepalives and partial endpoint frames retain cancellation.
// Endpoint validation stays with the SDK, which closes the body on invalid
// events or failed initialize. Each byte is scanned once, with bounded storage.
func (b *sseLifetimeBody) observeEndpoint(data []byte) error {
	for _, c := range data {
		if len(b.partial) == sseHandshakeEventLimit {
			return fmt.Errorf("SSE handshake event exceeds %d bytes", sseHandshakeEventLimit)
		}
		b.partial = append(b.partial, c)
		if !bytes.HasSuffix(b.partial, []byte("\n\n")) && !bytes.HasSuffix(b.partial, []byte("\r\n\r\n")) {
			continue
		}
		name, foundName, hasData := "", false, false
		for line := range strings.SplitSeq(string(b.partial), "\n") {
			line = strings.TrimRight(line, "\r")
			if value, ok := strings.CutPrefix(line, "event:"); ok && !foundName {
				name, foundName = strings.TrimSpace(value), true
			}
			if strings.HasPrefix(line, "data:") {
				hasData = true
			}
		}
		b.partial = b.partial[:0]
		if name == "endpoint" && hasData {
			b.established = true
			b.partial = nil
			b.stop()
			return nil
		}
	}
	return nil
}

// Match go-mcp's ordinary static-header/timeout client policy, changing only
// SSE response-body lifetime. Service credential builders retain their own
// entry-specific authentication/redirect/forwarded-context policy.
func ordinarySSEHTTPClient(headers map[string]string, seconds int) *http.Client {
	client := &http.Client{Transport: &ordinarySSEHeaders{base: &sseLifetimeTransport{base: http.DefaultTransport}, headers: maps.Clone(headers)}}
	if seconds > 0 {
		client.Timeout = time.Duration(seconds) * time.Second
	}
	return client
}

type ordinarySSEHeaders struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t *ordinarySSEHeaders) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	for name, value := range t.headers {
		out.Header.Set(name, value)
	}
	return t.base.RoundTrip(out)
}
