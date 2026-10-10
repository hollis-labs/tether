package federation

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hollis-labs/go-ssekit"
	messaging "github.com/hollis-labs/substrate/mesh/messaging"
)

type ssePeerTransport func(*http.Request) (*http.Response, error)

func (f ssePeerTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func ssePeer(t *testing.T, body io.ReadCloser) messaging.Store {
	t.Helper()
	store, err := HTTPDialer(&http.Client{Transport: ssePeerTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: body, Request: r}, nil
	})})(Peer{Authority: "test", BaseURL: "http://peer.test"})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestPeerSubscribeSSEFraming(t *testing.T) {
	for _, tc := range []struct {
		name, stream string
		want         []string
	}{
		{"multiline", "event: message\ndata:{\"id\":\"one\",\ndata: \"kind\":\"notice\"}\n\n", []string{"one"}},
		{"crlf", "data:{\"id\":\"one\"}\r\n\r\n", []string{"one"}},
		{"lone_cr", "data:{\"id\":\"one\"}\r\r", []string{"one"}},
		{"bom_and_control", "\xef\xbb\xbf: ping\nid: opaque\nretry: 10\nevent: ignored\n\ndata:{\"id\":\"one\"}\n\n", []string{"one"}},
		{"consecutive", "data:{\"id\":\"one\"}\n\ndata: {\"id\":\"two\"}\n\n", []string{"one", "two"}},
		{"incomplete_line", "data: {\"id\":\"lost\"}", nil},
		{"incomplete_block", "data: {\"id\":\"lost\"}\n", nil},
		{"complete_then_incomplete", "data:{\"id\":\"one\"}\n\ndata: {\"id\":\"lost\"}\n", []string{"one"}},
		{"malformed_then_valid", "data: not-json\n\ndata: {\"id\":\"one\"}\n\n", []string{"one"}},
		{"oversized_block", "data: " + strings.Repeat("x", ssekit.DefaultMaxEventBytes) + "\n\ndata: {\"id\":\"lost\"}\n\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ch, err := ssePeer(t, io.NopCloser(strings.NewReader(tc.stream))).Subscribe(context.Background(), addr("test", "recipient"), messaging.Filter{})
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for env := range ch {
				got = append(got, env.ID)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("envelopes = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPeerSubscribeWaitsForBlankLine(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reader, writer := io.Pipe()
		defer func() { _ = reader.Close() }()
		defer func() { _ = writer.Close() }()
		ch, err := ssePeer(t, reader).Subscribe(context.Background(), addr("test", "recipient"), messaging.Filter{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(writer, "data: {\"id\":\"one\"}\n"); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		select {
		case env := <-ch:
			t.Fatalf("dispatched before blank line: %+v", env)
		default:
		}
		if _, err := io.WriteString(writer, "\n"); err != nil {
			t.Fatal(err)
		}
		if env := <-ch; env.ID != "one" {
			t.Fatalf("envelope after blank line: %+v", env)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if env, ok := <-ch; ok {
			t.Fatalf("extra envelope: %+v", env)
		}
	})
}

func TestPeerSubscribeCancellationClosesIncompleteStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "data: {\"id\":\"unfinished\"}\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := peerStore(t, srv.URL).Subscribe(ctx, addr("test", "recipient"), messaging.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case env, ok := <-ch:
		if ok {
			t.Fatalf("dispatched incomplete event during cancellation: %+v", env)
		}
	case <-time.After(time.Second):
		t.Fatal("Subscribe did not close on cancellation")
	}
}
