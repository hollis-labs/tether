package runtime

import (
	"bytes"
	"io"
	"sync"
	"testing"
	"time"
)

// drainN reads up to n chunks from ch or returns what it got after timeout.
func drainN(ch <-chan []byte, n int, timeout time.Duration) []byte {
	deadline := time.After(timeout)
	var buf bytes.Buffer
	for i := 0; i < n; i++ {
		select {
		case chunk, ok := <-ch:
			if !ok {
				return buf.Bytes()
			}
			buf.Write(chunk)
		case <-deadline:
			return buf.Bytes()
		}
	}
	return buf.Bytes()
}

func TestAttachBroker_FanoutToSubscribers(t *testing.T) {
	b := newAttachBroker(1024, 16)

	_, ch1, cancel1 := b.subscribe(16)
	defer cancel1()
	_, ch2, cancel2 := b.subscribe(16)
	defer cancel2()

	if _, err := b.Write([]byte("hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got1 := drainN(ch1, 1, 200*time.Millisecond)
	got2 := drainN(ch2, 1, 200*time.Millisecond)
	if string(got1) != "hello" || string(got2) != "hello" {
		t.Fatalf("subscribers got %q / %q, want both %q", got1, got2, "hello")
	}
}

func TestAttachBroker_ReplayBeforeLive(t *testing.T) {
	b := newAttachBroker(1024, 16)

	if _, err := b.Write([]byte("pre-")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write([]byte("amble")); err != nil {
		t.Fatal(err)
	}

	replay, ch, cancel := b.subscribe(16)
	defer cancel()
	if string(replay) != "pre-amble" {
		t.Fatalf("replay = %q, want %q", replay, "pre-amble")
	}

	if _, err := b.Write([]byte(" live")); err != nil {
		t.Fatal(err)
	}
	live := drainN(ch, 1, 200*time.Millisecond)
	if string(live) != " live" {
		t.Fatalf("live bytes = %q, want %q", live, " live")
	}
}

func TestAttachBroker_RingBufferBoundedTrimsOldest(t *testing.T) {
	b := newAttachBroker(4, 16) // tiny ring buffer

	_, _ = b.Write([]byte("abcd"))
	_, _ = b.Write([]byte("efgh"))
	replay, _, cancel := b.subscribe(16)
	defer cancel()

	if string(replay) != "efgh" {
		t.Fatalf("replay after overflow = %q, want %q", replay, "efgh")
	}
}

func TestAttachBroker_RingBufferAcceptsOversizedWrite(t *testing.T) {
	b := newAttachBroker(4, 16)

	// Write larger than capacity: ring should keep the trailing 4 bytes.
	_, _ = b.Write([]byte("0123456789"))
	replay, _, cancel := b.subscribe(16)
	defer cancel()
	if string(replay) != "6789" {
		t.Fatalf("replay = %q, want %q", replay, "6789")
	}
}

func TestAttachBroker_UnsubscribeKeepsOthersAlive(t *testing.T) {
	b := newAttachBroker(1024, 16)

	_, ch1, cancel1 := b.subscribe(16)
	_, ch2, cancel2 := b.subscribe(16)
	defer cancel2()

	cancel1()

	if _, err := b.Write([]byte("after-drop")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := drainN(ch2, 1, 200*time.Millisecond)
	if string(got) != "after-drop" {
		t.Fatalf("surviving subscriber got %q, want %q", got, "after-drop")
	}

	// Cancelled channel should be closed (read returns with !ok).
	select {
	case _, ok := <-ch1:
		if ok {
			t.Fatal("expected cancelled ch1 to be closed")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("cancelled ch1 did not close")
	}
}

func TestAttachBroker_CloseSignalsSubscribers(t *testing.T) {
	b := newAttachBroker(1024, 16)
	_, ch, cancel := b.subscribe(16)
	defer cancel()

	b.close()

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected closed broker to close subscriber channel")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("close did not propagate to subscriber")
	}

	// Write after close returns io.ErrClosedPipe without panic.
	if _, err := b.Write([]byte("x")); err != io.ErrClosedPipe {
		t.Fatalf("Write after close = %v, want io.ErrClosedPipe", err)
	}

	// Second close is a no-op.
	b.close()
}

func TestAttachBroker_SubscribeAfterCloseReturnsClosedChan(t *testing.T) {
	b := newAttachBroker(1024, 16)
	_, _ = b.Write([]byte("history"))
	b.close()

	replay, ch, cancel := b.subscribe(16)
	defer cancel()

	if string(replay) != "history" {
		t.Fatalf("replay after close = %q, want %q", replay, "history")
	}
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected ch to be closed for post-close subscribe")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("post-close subscribe should return a closed channel")
	}
}

func TestAttachBroker_SlowSubscriberDoesNotBlockWriter(t *testing.T) {
	b := newAttachBroker(1024, 2) // small subscriber buffer

	// Subscribe but never read.
	_, _, cancel := b.subscribe(2)
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			_, _ = b.Write([]byte("xx"))
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("writer blocked by slow subscriber")
	}
}

func TestAttachBroker_ConcurrentSubscribePublishNoRace(t *testing.T) {
	b := newAttachBroker(4096, 64)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Publishers.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_, _ = b.Write([]byte("abc"))
				}
			}
		}()
	}

	// Subscribers that churn (subscribe, read a bit, detach).
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < 20; k++ {
				_, ch, cancel := b.subscribe(16)
				go func() {
					for range ch {
					}
				}()
				time.Sleep(time.Millisecond)
				cancel()
			}
		}()
	}

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
	b.close()
}

func TestComputeReplay_ZeroSinceSeqReturnsFullRing(t *testing.T) {
	ring := []byte("hello world")
	got := computeReplay(ring, 11, 0)
	if string(got) != "hello world" {
		t.Errorf("got %q, want full ring", got)
	}
}

func TestComputeReplay_AtHead_ReturnsEmpty(t *testing.T) {
	got := computeReplay([]byte("abc"), 3, 3)
	if len(got) != 0 {
		t.Errorf("got %q, want empty at head", got)
	}
}

func TestComputeReplay_PastHead_ReturnsEmpty(t *testing.T) {
	got := computeReplay([]byte("abc"), 3, 10)
	if len(got) != 0 {
		t.Errorf("got %q, want empty when sinceSeq>totalWritten", got)
	}
}

func TestComputeReplay_InsideRing_ReturnsTail(t *testing.T) {
	// Total 20 bytes written, ring holds the last 11 ("llo world!!"),
	// client requests from byte 15 — the tail should be bytes [15..20) = "rld!!".
	ring := []byte("llo world!!")
	got := computeReplay(ring, 20, 15)
	if string(got) != "rld!!" {
		t.Errorf("got %q, want 'rld!!'", got)
	}
}

func TestComputeReplay_GapBeforeRing_ReturnsFullRing(t *testing.T) {
	// Client requested from byte 3, ring only holds bytes [10..20).
	// Gap is silent: we return the full ring and let the caller
	// detect the gap by byte-count comparison.
	ring := []byte("0123456789")
	got := computeReplay(ring, 20, 3)
	if string(got) != "0123456789" {
		t.Errorf("got %q, want full ring on gap", got)
	}
}

func TestAttachBroker_SubscribeSince_LivesUpdate(t *testing.T) {
	b := newAttachBroker(1024, 16)
	// Write some history.
	b.Write([]byte("ABCDEFGHIJ")) // totalWritten=10
	// Subscribe asking only for bytes past offset 10 — should replay empty.
	replay, ch, cancel := b.subscribeSince(16, 10)
	defer cancel()
	if len(replay) != 0 {
		t.Fatalf("expected empty replay, got %q", replay)
	}
	// Subsequent writes are delivered live.
	go b.Write([]byte("KLM"))
	got := drainN(ch, 1, 500*time.Millisecond)
	if string(got) != "KLM" {
		t.Errorf("live got %q, want KLM", got)
	}
}

func TestAttachBroker_SubscribeSince_PartialReplay(t *testing.T) {
	b := newAttachBroker(1024, 16)
	b.Write([]byte("ABCDEFGHIJ")) // totalWritten=10
	// Client says "I have up to byte 6" — tail should be "GHIJ".
	replay, _, cancel := b.subscribeSince(16, 6)
	defer cancel()
	if string(replay) != "GHIJ" {
		t.Errorf("partial replay = %q, want GHIJ", replay)
	}
}
