package federation

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	messaging "github.com/hollis-labs/go-messaging"
	otelprop "github.com/hollis-labs/go-otel/propagation"
)

// ErrWrongRecipient is returned by a peer store when a Consume targets an
// envelope the caller is not addressed as. It mirrors the local store's
// 409 Conflict semantics across the federation hop.
var ErrWrongRecipient = errors.New("federation: caller is not the intended recipient")

// Dialer turns a configured Peer into the messaging.Store that reaches it.
// It is the seam for swapping transports: HTTPDialer ships plain HTTP, and
// a hardened mTLS dialer (program task M2) can be slotted in without
// touching the Router or BuildRouter.
type Dialer func(p Peer) (messaging.Store, error)

// HTTPDialer returns a Dialer that builds an HTTP-backed messaging.Store
// over a peer daemon's go-messaging /messages/* routes. A nil client uses
// http.DefaultClient.
//
// The client MUST NOT carry a short Timeout: Subscribe holds a long-lived
// SSE connection and a request timeout would sever it. Use per-call
// context deadlines instead. Authenticating the hop (mTLS, signed
// envelopes) is program task M2 — supply a client with a hardened
// Transport here when that lands.
func HTTPDialer(client *http.Client) Dialer {
	return func(p Peer) (messaging.Store, error) {
		base, err := url.Parse(p.BaseURL)
		if err != nil {
			return nil, fmt.Errorf("federation: peer %q base_url: %w", p.Authority, err)
		}
		c := client
		if c == nil {
			c = http.DefaultClient
		}
		return &httpPeerStore{base: base, client: c, authority: p.Authority}, nil
	}
}

// httpPeerStore implements messaging.Store over a remote daemon's
// /messages/* HTTP surface — the federation hop. It is the cross-host
// counterpart of Tether's own internal/api message routes, so a Tether
// peer reaches another Tether (or any go-messaging-native) daemon.
type httpPeerStore struct {
	base      *url.URL
	client    *http.Client
	authority string
}

var _ messaging.Store = (*httpPeerStore)(nil)

func (s *httpPeerStore) url(elem ...string) string {
	return s.base.JoinPath(elem...).String()
}

// do issues req and maps a non-2xx status to a sentinel or descriptive
// error. On success it leaves the response body open for the caller.
func (s *httpPeerStore) do(req *http.Request) (*http.Response, error) {
	otelprop.InjectHTTP(req.Context(), req)
	// The request URL is built from the peer's operator-configured,
	// Config.Validate-checked base URL — not from envelope content — so
	// this is not an SSRF sink.
	resp, err := s.client.Do(req) //nolint:gosec // G704: peer base URL is operator-configured + validated
	if err != nil {
		return nil, fmt.Errorf("federation: peer %q unreachable: %w", s.authority, err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotFound:
		return nil, messaging.ErrNotFound
	case http.StatusConflict:
		return nil, ErrWrongRecipient
	default:
		return nil, fmt.Errorf("federation: peer %q returned %s: %s",
			s.authority, resp.Status, strings.TrimSpace(string(body)))
	}
}

func (s *httpPeerStore) Send(ctx context.Context, env messaging.Envelope) (messaging.Envelope, error) {
	if env.DeliveredAt != nil || env.ConsumedAt != nil {
		return messaging.Envelope{}, messaging.ErrPresetLifecycle
	}
	// The remote daemon assigns id/created_at; clear caller values so the
	// wire request matches the local-store contract.
	env.ID = ""
	body, err := json.Marshal(env)
	if err != nil {
		return messaging.Envelope{}, fmt.Errorf("federation: marshal envelope: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url("messages"), bytes.NewReader(body))
	if err != nil {
		return messaging.Envelope{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.do(req)
	if err != nil {
		return messaging.Envelope{}, err
	}
	defer resp.Body.Close() //nolint:errcheck
	var out messaging.Envelope
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return messaging.Envelope{}, fmt.Errorf("federation: decode send response: %w", err)
	}
	return out, nil
}

func (s *httpPeerStore) Get(ctx context.Context, id string) (messaging.Envelope, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url("messages", id), nil)
	if err != nil {
		return messaging.Envelope{}, err
	}
	resp, err := s.do(req)
	if err != nil {
		return messaging.Envelope{}, err
	}
	defer resp.Body.Close() //nolint:errcheck
	var out messaging.Envelope
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return messaging.Envelope{}, fmt.Errorf("federation: decode get response: %w", err)
	}
	return out, nil
}

func (s *httpPeerStore) Inbox(ctx context.Context, to messaging.Address, f messaging.Filter) ([]messaging.Envelope, error) {
	u := s.base.JoinPath("messages", "inbox")
	q := url.Values{}
	q.Set("to", to.URN())
	// T07 (messaging vNext): the peer's GET /messages/inbox has required
	// ?as= matching ?to= since T05 (ADR 0045) -- this client never sent it,
	// so every federated Inbox call has 400'd against a real peer since
	// that change landed. `to` is the mailbox being read, so it doubles as
	// the claim, matching internal/client.Client.MessageInbox's identical
	// convention for the local (non-federated) client.
	q.Set("as", to.URN())
	applyFilter(q, f)
	u.RawQuery = q.Encode()
	return s.fetchEnvelopes(ctx, u.String())
}

func (s *httpPeerStore) Thread(ctx context.Context, threadID string, f messaging.Filter) ([]messaging.Envelope, error) {
	u := s.base.JoinPath("messages", "thread", threadID)
	q := url.Values{}
	applyFilter(q, f)
	u.RawQuery = q.Encode()
	return s.fetchEnvelopes(ctx, u.String())
}

// fetchEnvelopes GETs a route that returns {"messages": [...]} and decodes
// the envelope slice.
func (s *httpPeerStore) fetchEnvelopes(ctx context.Context, fullURL string) ([]messaging.Envelope, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck
	var out struct {
		Messages []messaging.Envelope `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("federation: decode envelope list: %w", err)
	}
	return out.Messages, nil
}

func (s *httpPeerStore) Consume(ctx context.Context, id string, recipient messaging.Address) error {
	u := s.base.JoinPath("messages", id, "consume")
	u.RawQuery = url.Values{"as": {recipient.URN()}}.Encode()
	return s.post(ctx, u.String())
}

func (s *httpPeerStore) Cancel(ctx context.Context, id string) error {
	return s.post(ctx, s.url("messages", id, "cancel"))
}

// post issues a bodyless POST and discards the (204) response.
func (s *httpPeerStore) post(ctx context.Context, fullURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, nil)
	if err != nil {
		return err
	}
	resp, err := s.do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return resp.Body.Close()
}

// Subscribe streams envelopes from the peer's /messages/subscribe SSE
// route. The returned channel closes when ctx is done or the stream ends.
func (s *httpPeerStore) Subscribe(ctx context.Context, to messaging.Address, f messaging.Filter) (<-chan messaging.Envelope, error) {
	u := s.base.JoinPath("messages", "subscribe")
	q := url.Values{}
	q.Set("to", to.URN())
	applyFilter(q, f)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := s.do(req)
	if err != nil {
		return nil, err
	}

	out := make(chan messaging.Envelope, 32)
	go func() {
		defer close(out)
		defer resp.Body.Close() //nolint:errcheck
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			// Tether emits one `data: <json>` line per "message" event;
			// `: ping` comments and `event:` lines are skipped.
			payload, ok := strings.CutPrefix(line, "data: ")
			if !ok {
				continue
			}
			var env messaging.Envelope
			if json.Unmarshal([]byte(payload), &env) != nil {
				continue
			}
			select {
			case out <- env:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// applyFilter writes the kind/thread_id/limit query params shared by the
// peer's inbox, thread, and subscribe routes.
func applyFilter(q url.Values, f messaging.Filter) {
	if len(f.Kind) > 0 {
		kinds := make([]string, len(f.Kind))
		for i, k := range f.Kind {
			kinds[i] = string(k)
		}
		q.Set("kind", strings.Join(kinds, ","))
	}
	if f.ThreadID != "" {
		q.Set("thread_id", f.ThreadID)
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
}
