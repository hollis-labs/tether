package e2e

// concurrent_claims_test.go — T11 "concurrent claims" scenario. Unit
// coverage already exists at the *sql.DB level
// (TestDeliveryStore_ConcurrentClaimsAcrossIndependentConnections). This
// promotes the same property to the real public surface: N real HTTP
// clients race POST /messages/{id}/claim (distinct holders) against the
// same message on a real running daemon; exactly one may win the lease.
//
// MessageConsume (the typed client's only claim-shaped method) is
// deliberately NOT used for this scenario: it is documented as idempotent
// per-recipient ("if the message was already consumed by this recipient,
// returns nil"), so racing the SAME recipient's Consume has no "exactly
// one winner" story to prove. /messages/{id}/claim has no typed client
// wrapper yet, so this test speaks raw HTTP over the daemon's real UDS
// socket directly -- still the real public wire surface, just without a
// Go convenience type in front of it.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/hollis-labs/tether/internal/daemon"
)

// claimResult is one goroutine's outcome from POST /messages/{id}/claim.
type claimResult struct {
	status int
	body   string
}

func rawClaim(t *testing.T, socketAddr, messageID, asURN, holder string) claimResult {
	t.Helper()
	httpClient := daemon.DialHTTPClient(socketAddr)
	url := daemon.BaseURL(socketAddr) + "/messages/" + messageID + "/claim?as=" + asURN
	body, err := json.Marshal(map[string]any{"holder": holder, "lease_seconds": 30})
	if err != nil {
		t.Fatalf("marshal claim body: %v", err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("POST claim: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return claimResult{status: resp.StatusCode, body: string(respBody)}
}

func TestConcurrentClaims_ExactlyOneClaimWinsAgainstRealDaemon(t *testing.T) {
	d := StartFixtureDaemon(t)
	c := d.Client()

	sender := registerAgent(t, c, "e2e-race-sender")
	recipient := registerAgent(t, c, "e2e-race-recipient")

	sent, err := c.MessageSend(ctx(), sendNotice(t, sender, recipient, "one winner only"))
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	const racers = 8
	var wg sync.WaitGroup
	results := make([]claimResult, racers)
	wg.Add(racers)
	for i := range racers {
		go func(i int) {
			defer wg.Done()
			results[i] = rawClaim(t, d.SocketAddr, sent.ID, recipient, "holder-"+string(rune('a'+i)))
		}(i)
	}
	wg.Wait()

	var wins, losses int
	for _, r := range results {
		if r.status == http.StatusOK {
			wins++
		} else {
			losses++
		}
	}
	if wins != 1 {
		t.Fatalf("wins = %d, want exactly 1 (losses = %d, results = %+v)", wins, losses, results)
	}
	if losses != racers-1 {
		t.Fatalf("losses = %d, want %d", losses, racers-1)
	}

	trace, err := c.MessageTrace(ctx(), sent.ID)
	if err != nil {
		t.Fatalf("trace: %v", err)
	}
	if trace.AttemptCount != 1 {
		t.Errorf("attempt_count = %d, want 1 -- exactly one claim should have created a durable attempt", trace.AttemptCount)
	}
	if trace.Status != "leased" {
		t.Errorf("status = %q, want leased", trace.Status)
	}
}
