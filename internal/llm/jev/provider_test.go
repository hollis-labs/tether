package jev

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/llm"
)

const testKey = "sk-test-SECRET-key-123"

func testRequest() llm.Request {
	return llm.Request{
		Operation: llm.OperationDecide,
		RequestID: "req-1",
		Decision: &llm.DecisionRequest{
			State:   map[string]any{"title": "fix login", "body": "500 on submit"},
			Purpose: "task-kind",
			OnError: llm.OnErrorFailOpen,
			Questions: []llm.DecisionQuestion{
				{Key: "kind", Type: llm.QuestionChoice, Instructions: "What kind of task?", Options: []llm.DecisionOption{
					{Key: "zeta", Description: "last alphabetically, first listed"},
					{Key: "bug", Description: "a defect"},
					{Key: "alpha"},
				}},
				{Key: "urgent", Type: llm.QuestionNoul, Instructions: "Is it urgent?", True: "needs action today", False: "can wait"},
				{Key: "size", Type: llm.QuestionScore, Instructions: "How big?", Levels: []string{"tiny", "small", "large"}},
			},
		},
	}
}

var route = llm.RouteDecision{Provider: "jev", Model: "jev-1.13.0"}

const okBody = `{"model":"jev-1.13.0","answers":{
 "kind":{"type":"choice","choice":"bug","confidence":0.8,"probabilities":{"zeta":0.1,"bug":0.8,"alpha":0.1}},
 "urgent":{"type":"noul","noul":0.9},
 "size":{"type":"score","score":1.4,"confidence":0.4,"legend":{"0":"tiny"},"probabilities":{"0":0,"1":0.6,"2":0.4}}},
 "usage":{"input_tokens":1000000,"output_tokens":12}}`

func newProvider(t *testing.T, srv *httptest.Server, mod func(*Config)) *Provider {
	t.Helper()
	cfg := Config{
		BaseURL:              srv.URL,
		ResolveAPIKey:        func(context.Context) (string, error) { return testKey, nil },
		InputPriceUSDPerMTok: 0.042,
		Sleep:                func(context.Context, time.Duration) error { return nil },
	}
	if mod != nil {
		mod(&cfg)
	}
	return New(cfg)
}

func TestDecideSuccessOrderedCriteriaOnWire(t *testing.T) {
	var gotBody []byte
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(okBody))
	}))
	defer srv.Close()

	resp, err := newProvider(t, srv, nil).Decide(context.Background(), testRequest(), route)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if gotPath != "/v1/systemone" || gotAuth != "Bearer "+testKey {
		t.Fatalf("path=%q auth ok=%v", gotPath, gotAuth == "Bearer "+testKey)
	}
	// Option order survives to the wire: zeta, bug, alpha (not sorted).
	body := string(gotBody)
	iz, ib, ia := strings.Index(body, `"zeta"`), strings.Index(body, `"bug"`), strings.Index(body, `"alpha"`)
	if iz < 0 || iz >= ib || ib >= ia {
		t.Fatalf("option order lost on the wire: %s", body)
	}
	if !strings.Contains(body, `"alpha":null`) {
		t.Fatalf("empty description should be null: %s", body)
	}
	if !strings.Contains(body, `"model":"jev-1.13.0"`) || !strings.Contains(body, `"criteria":["tiny","small","large"]`) {
		t.Fatalf("unexpected body: %s", body)
	}
	if !strings.Contains(body, `"criteria":{"true":"needs action today","false":"can wait"}`) {
		t.Fatalf("noul criteria missing: %s", body)
	}
	// Question order is preserved too.
	if strings.Index(body, `"kind"`) >= strings.Index(body, `"urgent"`) || strings.Index(body, `"urgent"`) >= strings.Index(body, `"size"`) {
		t.Fatalf("question order lost: %s", body)
	}

	d := resp.Decision
	if d == nil || d.Answers["kind"].Choice != "bug" || d.Answers["urgent"].Noul != 0.9 || d.Answers["size"].Score != 1.4 {
		t.Fatalf("answers: %+v", d)
	}
	if d.Answers["urgent"].Confidence != nil {
		t.Fatal("noul must carry no invented confidence")
	}
	if c := d.Answers["kind"].Confidence; c == nil || *c != 0.8 {
		t.Fatalf("choice confidence: %v", c)
	}
	if resp.Provider != "jev" || resp.Model != "jev-1.13.0" || d.RequestID != "req-1" {
		t.Fatalf("envelope: %+v", resp)
	}
	if resp.Usage.InputTokens != 1000000 || resp.Usage.OutputTokens != 12 {
		t.Fatalf("usage: %+v", resp.Usage)
	}
	if got := resp.Usage.EstimatedCostUSD; got < 0.0419 || got > 0.0421 {
		t.Fatalf("cost: %v", got)
	}
}

func TestDecideStatusErrors(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		wantKind  llm.DecisionErrorKind
		wantCalls int32
	}{
		{"401", 401, llm.DecisionErrAuth, 1},
		{"422 never retried", 422, llm.DecisionErrInvalid, 1},
		{"429 retried then given up", 429, llm.DecisionErrRateLimited, 4},
		{"529 retried then given up", 529, llm.DecisionErrOverloaded, 4},
		{"500", 500, llm.DecisionErrUnavailable, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&calls, 1)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"detail":"questions.kind.criteria is required"}`))
			}))
			defer srv.Close()
			_, err := newProvider(t, srv, nil).Decide(context.Background(), testRequest(), route)
			var de *llm.DecisionError
			if !errors.As(err, &de) {
				t.Fatalf("want DecisionError, got %v", err)
			}
			if de.Kind != tc.wantKind || de.Status != tc.status || de.OnError != llm.OnErrorFailOpen {
				t.Fatalf("got %+v", de)
			}
			if calls != tc.wantCalls {
				t.Fatalf("calls=%d want %d", calls, tc.wantCalls)
			}
			if strings.Contains(err.Error(), testKey) {
				t.Fatal("key leaked in error")
			}
			if tc.status == 422 && !strings.Contains(err.Error(), "questions.kind.criteria") {
				t.Fatalf("422 should name the field: %v", err)
			}
		})
	}
}

func TestDecideRetrySucceedsAfter429(t *testing.T) {
	var calls int32
	var slept []time.Duration
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(429)
			return
		}
		_, _ = w.Write([]byte(okBody))
	}))
	defer srv.Close()
	p := newProvider(t, srv, func(c *Config) {
		c.Sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
		c.Backoff = 100 * time.Millisecond
	})
	if _, err := p.Decide(context.Background(), testRequest(), route); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if calls != 3 || len(slept) != 2 || slept[1] != 2*slept[0] {
		t.Fatalf("calls=%d slept=%v (want exponential)", calls, slept)
	}
}

func TestDecideTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	p := newProvider(t, srv, func(c *Config) { c.Timeout = 50 * time.Millisecond })
	_, err := p.Decide(context.Background(), testRequest(), route)
	var de *llm.DecisionError
	if !errors.As(err, &de) || de.Kind != llm.DecisionErrTimeout || de.OnError != llm.OnErrorFailOpen {
		t.Fatalf("got %v", err)
	}
}

func TestDecideCircuitBreaker(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(500)
	}))
	defer srv.Close()
	now := time.Unix(1000, 0)
	p := newProvider(t, srv, func(c *Config) {
		c.BreakerThreshold = 2
		c.BreakerCooldown = time.Minute
		c.Now = func() time.Time { return now }
	})
	for i := 0; i < 2; i++ {
		_, _ = p.Decide(context.Background(), testRequest(), route)
	}
	_, err := p.Decide(context.Background(), testRequest(), route)
	var de *llm.DecisionError
	if !errors.As(err, &de) || de.Kind != llm.DecisionErrCircuitOpen {
		t.Fatalf("want circuit_open, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("open breaker must not call the provider; calls=%d", calls)
	}
	now = now.Add(2 * time.Minute)
	_, _ = p.Decide(context.Background(), testRequest(), route)
	if calls != 3 {
		t.Fatalf("probe after cooldown expected; calls=%d", calls)
	}
}

func TestDecideInvalidRequestNeverCallsProvider(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&calls, 1) }))
	defer srv.Close()
	req := testRequest()
	req.Decision.Questions[2].Levels = []string{"only one"}
	_, err := newProvider(t, srv, nil).Decide(context.Background(), req, route)
	if !errors.Is(err, llm.ErrInvalidDecision) || calls != 0 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestDecideMalformedAnswersAreUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{"urgent":{"type":"noul","noul":0.5}},"usage":{}}`))
	}))
	defer srv.Close()
	_, err := newProvider(t, srv, nil).Decide(context.Background(), testRequest(), route)
	var de *llm.DecisionError
	if !errors.As(err, &de) || de.Kind != llm.DecisionErrUnavailable {
		t.Fatalf("got %v", err)
	}
}

func TestDecideKeyNeverLogged(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(io.Discard)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer srv.Close()
	_, err := newProvider(t, srv, nil).Decide(context.Background(), testRequest(), route)
	if err == nil || strings.Contains(err.Error(), testKey) || strings.Contains(buf.String(), testKey) {
		t.Fatalf("key leaked: err=%v log=%q", err, buf.String())
	}

	// A resolver failure that echoes the secret ref must not surface.
	p := New(Config{BaseURL: srv.URL, ResolveAPIKey: func(context.Context) (string, error) {
		return "", errors.New("helper failed for keychain://typesafe/jev")
	}})
	_, err = p.Decide(context.Background(), testRequest(), route)
	var de *llm.DecisionError
	if !errors.As(err, &de) || de.Kind != llm.DecisionErrAuth || strings.Contains(err.Error(), "keychain://") {
		t.Fatalf("got %v", err)
	}
}
