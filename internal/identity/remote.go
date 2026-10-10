package identity

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
)

const PairingExchangePath = "/auth/pair/exchange"

type remoteKey struct{}
type credentialHashKey struct{}

func IsRemote(ctx context.Context) bool { remote, _ := ctx.Value(remoteKey{}).(bool); return remote }
func CurrentCredentialHash(ctx context.Context) string {
	hash, _ := ctx.Value(credentialHashKey{}).(string)
	return hash
}

// WithRemoteContext marks server-owned remote provenance. Tests may construct a synthetic
// request context; headers, query strings and payloads cannot set this marker.
func WithRemoteContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, remoteKey{}, true)
}

func remoteMiddleware(verifier Verifier, record func(context.Context, Observation) error, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		r = r.WithContext(WithRemoteContext(r.Context()))
		// The one remote bootstrap exception is exact, POST-only and remains
		// behind the listener's Host/Origin and protocol gates. The exchange
		// handler supplies body bounds, a global rate limit and no-cache output.
		if r.Method == http.MethodPost && r.URL.Path == PairingExchangePath {
			next.ServeHTTP(w, r)
			return
		}
		guard := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/health" {
				next.ServeHTTP(w, r)
				return
			}
			p, ok := FromContext(r.Context())
			store, stored := verifier.(*Store)
			if !ok || p.Kind != "device" || p.ID == OperatorID || !stored {
				authError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			parts := strings.Fields(r.Header.Get("Authorization"))
			if len(parts) != 2 {
				authError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			hash := HashToken(parts[1])
			ctx, stop, err := store.watchDevice(r.Context(), p, hash)
			if err != nil {
				if errors.Is(err, ErrInvalidToken) {
					authError(w, http.StatusUnauthorized, "unauthorized")
				} else {
					authError(w, http.StatusServiceUnavailable, "identity_unavailable")
				}
				return
			}
			defer stop()
			ctx = context.WithValue(ctx, credentialHashKey{}, hash)
			// A canceled stream must also unblock an HTTP writer waiting on a
			// non-reading peer, without stopping its hosted agent or shim.
			done := make(chan struct{})
			joined := make(chan struct{})
			go func() {
				defer close(joined)
				select {
				case <-ctx.Done():
					if deviceStreamRequest(r) {
						_ = http.NewResponseController(w).SetWriteDeadline(time.Now())
					}
				case <-done:
				}
			}()
			defer func() { close(done); <-joined }()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
		remoteRecord := func(ctx context.Context, o Observation) error {
			if o.Authentication != "verified" {
				if record != nil {
					return record(ctx, o)
				}
				return nil
			}
			p, ok := FromContext(ctx)
			store, stored := verifier.(*Store)
			if !ok || !stored {
				return fmt.Errorf("device audit unavailable")
			}
			return store.RecordDeviceUse(ctx, p, o, r.RemoteAddr, r.UserAgent())
		}
		middleware(Enforce, verifier, remoteRecord, guard, true).ServeHTTP(w, r)
	})
}

func deviceStreamRequest(r *http.Request) bool {
	p := r.URL.Path
	if p == "/events/stream" || p == "/environment/events" || p == "/messages/subscribe" || p == "/ai/chat/stream" {
		return true
	}
	if strings.HasPrefix(p, "/sessions/") && (strings.HasSuffix(p, "/stream") || strings.HasSuffix(p, "/events") || strings.HasSuffix(p, "/attach")) {
		return true
	}
	return strings.HasPrefix(p, "/channels/") && strings.HasSuffix(p, "/subscribe") || strings.HasPrefix(p, "/a2a/") && strings.HasSuffix(p, "/rpc")
}

type deviceStream struct {
	cancel context.CancelFunc
	p      Principal
	hash   string
}

func (s *Store) watchDevice(parent context.Context, p Principal, hash string) (context.Context, func(), error) {
	ctx, cancel := context.WithCancel(parent)
	stream := &deviceStream{cancel: cancel, p: p, hash: hash}
	s.streamMu.Lock()
	if s.streams == nil {
		s.streams = make(map[string]map[*deviceStream]struct{})
	}
	if s.streams[p.ID] == nil {
		s.streams[p.ID] = make(map[*deviceStream]struct{})
	}
	s.streams[p.ID][stream] = struct{}{}
	s.streamMu.Unlock()
	stop := func() {
		cancel()
		s.streamMu.Lock()
		delete(s.streams[p.ID], stream)
		if len(s.streams[p.ID]) == 0 {
			delete(s.streams, p.ID)
		}
		s.streamMu.Unlock()
	}
	if err := s.verifyDeviceStream(ctx, stream); err != nil {
		stop()
		return nil, nil, err
	}
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !s.deviceStreamValid(ctx, stream) {
					cancel()
					return
				}
			}
		}
	}()
	return ctx, func() { stop(); <-joined }, nil
}

func (s *Store) deviceStreamValid(ctx context.Context, stream *deviceStream) bool {
	return s.verifyDeviceStream(ctx, stream) == nil
}

func (s *Store) verifyDeviceStream(ctx context.Context, stream *deviceStream) error {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	p, err := s.verifyHash(ctx, stream.hash)
	if err != nil {
		return err
	}
	if p.Kind != "device" || p.ID != stream.p.ID || !slices.Equal(p.Scopes, stream.p.Scopes) {
		return ErrInvalidToken
	}
	return nil
}

func (s *Store) cancelDeviceStreams(id string) {
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	for stream := range s.streams[id] {
		stream.cancel()
	}
}

func (s *Store) checkDeviceStreams() {
	s.streamMu.Lock()
	var active []*deviceStream
	for _, streams := range s.streams {
		for stream := range streams {
			active = append(active, stream)
		}
	}
	s.streamMu.Unlock()
	for _, stream := range active {
		if !s.deviceStreamValid(context.Background(), stream) {
			stream.cancel()
		}
	}
}
