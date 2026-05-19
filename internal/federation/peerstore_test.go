package federation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	messaging "github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/go-messaging/memstore"
)

// fakeDaemon mounts the subset of a daemon's /messages/* HTTP surface that
// the federation peer store calls, backed by an in-memory store. It is the
// stand-in for a remote Tether/Torque daemon across the federation hop.
func fakeDaemon(t *testing.T) (*httptest.Server, *memstore.Store) {
	t.Helper()
	ms := memstore.New()
	mux := http.NewServeMux()

	writeEnvelopes := func(w http.ResponseWriter, envs []messaging.Envelope) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"messages": envs})
	}
	fail := func(w http.ResponseWriter, err error) {
		if errors.Is(err, messaging.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}

	mux.HandleFunc("/messages", func(w http.ResponseWriter, r *http.Request) {
		var env messaging.Envelope
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		sent, err := ms.Send(r.Context(), env)
		if err != nil {
			fail(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(sent)
	})

	mux.HandleFunc("/messages/inbox", func(w http.ResponseWriter, r *http.Request) {
		to, err := messaging.ParseURN(r.URL.Query().Get("to"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		envs, err := ms.Inbox(r.Context(), to, queryFilter(r))
		if err != nil {
			fail(w, err)
			return
		}
		writeEnvelopes(w, envs)
	})

	mux.HandleFunc("/messages/thread/", func(w http.ResponseWriter, r *http.Request) {
		threadID := strings.TrimPrefix(r.URL.Path, "/messages/thread/")
		envs, err := ms.Thread(r.Context(), threadID, queryFilter(r))
		if err != nil {
			fail(w, err)
			return
		}
		writeEnvelopes(w, envs)
	})

	mux.HandleFunc("/messages/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/messages/")
		parts := strings.SplitN(rest, "/", 2)
		id := parts[0]
		action := ""
		if len(parts) == 2 {
			action = parts[1]
		}
		switch action {
		case "":
			env, err := ms.Get(r.Context(), id)
			if err != nil {
				fail(w, err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(env)
		case "consume":
			recipient, err := messaging.ParseURN(r.URL.Query().Get("as"))
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			env, err := ms.Get(r.Context(), id)
			if err != nil {
				fail(w, err)
				return
			}
			if env.To.URN() != recipient.URN() {
				http.Error(w, "wrong recipient", http.StatusConflict)
				return
			}
			if err := ms.Consume(r.Context(), id, recipient); err != nil {
				fail(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case "cancel":
			if err := ms.Cancel(r.Context(), id); err != nil {
				fail(w, err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unknown action", http.StatusNotFound)
		}
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, ms
}

func queryFilter(r *http.Request) messaging.Filter {
	var f messaging.Filter
	if ks := r.URL.Query().Get("kind"); ks != "" {
		for _, k := range strings.Split(ks, ",") {
			f.Kind = append(f.Kind, messaging.Kind(strings.TrimSpace(k)))
		}
	}
	f.ThreadID = r.URL.Query().Get("thread_id")
	return f
}

func peerStore(t *testing.T, baseURL string) messaging.Store {
	t.Helper()
	st, err := HTTPDialer(nil)(Peer{Authority: "torque", BaseURL: baseURL})
	if err != nil {
		t.Fatalf("HTTPDialer: %v", err)
	}
	return st
}

func TestPeerStoreSendGet(t *testing.T) {
	ctx := context.Background()
	srv, ms := fakeDaemon(t)
	ps := peerStore(t, srv.URL)

	sent, err := ps.Send(ctx, envTo(addr("torque", "worker")))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if sent.ID == "" {
		t.Fatal("Send did not return a server-assigned id")
	}
	if _, err := ms.Get(ctx, sent.ID); err != nil {
		t.Fatalf("remote store missing the sent envelope: %v", err)
	}

	got, err := ps.Get(ctx, sent.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != sent.ID || got.To.URN() != sent.To.URN() {
		t.Fatalf("Get round-trip mismatch: got %+v", got)
	}
}

func TestPeerStoreInboxAndThread(t *testing.T) {
	ctx := context.Background()
	srv, _ := fakeDaemon(t)
	ps := peerStore(t, srv.URL)

	to := addr("torque", "worker")
	env := envTo(to)
	env.ThreadID = "thread-1"
	if _, err := ps.Send(ctx, env); err != nil {
		t.Fatalf("Send: %v", err)
	}

	inbox, err := ps.Inbox(ctx, to, messaging.Filter{})
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if len(inbox) != 1 {
		t.Fatalf("Inbox returned %d envelopes, want 1", len(inbox))
	}

	thread, err := ps.Thread(ctx, "thread-1", messaging.Filter{})
	if err != nil {
		t.Fatalf("Thread: %v", err)
	}
	if len(thread) != 1 || thread[0].ThreadID != "thread-1" {
		t.Fatalf("Thread returned %d envelopes, want the one in thread-1", len(thread))
	}
}

func TestPeerStoreConsumeAndCancel(t *testing.T) {
	ctx := context.Background()
	srv, _ := fakeDaemon(t)
	ps := peerStore(t, srv.URL)

	to := addr("torque", "worker")
	sent, err := ps.Send(ctx, envTo(to))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := ps.Consume(ctx, sent.ID, to); err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if err := ps.Cancel(ctx, sent.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
}

func TestPeerStoreErrorMapping(t *testing.T) {
	ctx := context.Background()
	srv, _ := fakeDaemon(t)
	ps := peerStore(t, srv.URL)

	if _, err := ps.Get(ctx, "no-such-id"); !errors.Is(err, messaging.ErrNotFound) {
		t.Fatalf("Get of a missing id: got %v, want ErrNotFound", err)
	}

	// Send to one recipient, then consume as a different one → 409.
	sent, err := ps.Send(ctx, envTo(addr("torque", "alice")))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	err = ps.Consume(ctx, sent.ID, addr("torque", "bob"))
	if !errors.Is(err, ErrWrongRecipient) {
		t.Fatalf("Consume as the wrong recipient: got %v, want ErrWrongRecipient", err)
	}
}

func TestPeerStoreRejectsPresetLifecycle(t *testing.T) {
	srv, _ := fakeDaemon(t)
	ps := peerStore(t, srv.URL)
	now := time.Now()
	env := envTo(addr("torque", "worker"))
	env.DeliveredAt = &now
	if _, err := ps.Send(context.Background(), env); !errors.Is(err, messaging.ErrPresetLifecycle) {
		t.Fatalf("Send with preset DeliveredAt: got %v, want ErrPresetLifecycle", err)
	}
}

func TestPeerStoreSubscribe(t *testing.T) {
	want := envTo(addr("torque", "worker"))
	want.ID = "evt-1"
	want.CreatedAt = time.Now().UTC()

	mux := http.NewServeMux()
	mux.HandleFunc("/messages/subscribe", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("test server does not support flushing")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		b, _ := json.Marshal(want)
		_, _ = w.Write([]byte("event: message\ndata: " + string(b) + "\n\n"))
		flusher.Flush()
		<-r.Context().Done()
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ps := peerStore(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := ps.Subscribe(ctx, addr("torque", "worker"), messaging.Filter{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	select {
	case got, ok := <-ch:
		if !ok {
			t.Fatal("Subscribe channel closed before delivering an event")
		}
		if got.ID != want.ID {
			t.Fatalf("Subscribe delivered id %q, want %q", got.ID, want.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a subscribed envelope")
	}
}
