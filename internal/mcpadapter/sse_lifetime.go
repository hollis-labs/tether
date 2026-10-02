package mcpadapter

import (
	"context"
	"io"
	"net/http"
)

// sseLifetimeTransport bounds dialing/header acquisition by the handshake
// context, while the established SSE body belongs to the SDK session. The SDK
// closes that body on failed initialization, disconnect and owner shutdown.
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
	if !stop() && req.Context().Err() != nil {
		_ = response.Body.Close()
		cancel()
		return nil, req.Context().Err()
	}
	response.Body = &sseLifetimeBody{ReadCloser: response.Body, cancel: cancel}
	return response, nil
}

type sseLifetimeBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *sseLifetimeBody) Close() error { b.cancel(); return b.ReadCloser.Close() }
func (b *sseLifetimeBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.cancel()
	}
	return n, err
}
