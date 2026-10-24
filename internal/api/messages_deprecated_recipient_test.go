package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/libs/mesh/core/messaging"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/testutil/migrateddb"
)

// failingLookup is a registry whose lookups fail with something other than
// not-found: a registry outage must never turn into refused mail.
type failingLookup struct {
	RegistryService
}

func (failingLookup) Lookup(context.Context, string) (registry.Profile, error) {
	return registry.Profile{}, errors.New("registry unavailable")
}

func deprecatedRecipientServer(t *testing.T, wrap func(*registry.Service) RegistryService) (http.Handler, *registry.Service, *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "msgs.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	svc := registry.NewService(registry.NewStorage(migrateddb.Open(t)))
	var reg RegistryService = svc
	if wrap != nil {
		reg = wrap(svc)
	}
	return NewHandler(Deps{Service: &fakeLaunchService{}, Registry: reg, MessageStore: db.MessagingStore()}), svc, db
}

func postMessage(h http.Handler, path, to string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]any{
		"from": "msg://user/local/operator", "to": to, "kind": "notice",
		"payload": map[string]any{"subject": "hello", "body": "are you there"},
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body)))
	return rr
}

func TestSendToDeprecatedAgentIsRefusedAndNothingIsStored(t *testing.T) {
	h, svc, db := deprecatedRecipientServer(t, nil)
	ctx := context.Background()
	agent, err := svc.Register(ctx, registry.KindAgent, registry.Profile{DisplayName: "Retiring"})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/messages", "/messages/notify"} {
		if rr := postMessage(h, path, agent.URN); rr.Code != http.StatusCreated {
			t.Fatalf("%s to an active agent: status %d: %s", path, rr.Code, rr.Body.String())
		}
	}
	if _, err := svc.Deregister(ctx, agent.URN); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/messages", "/messages/notify"} {
		rr := postMessage(h, path, agent.URN)
		if rr.Code != http.StatusConflict {
			t.Fatalf("%s to a deprecated agent: status %d, want 409: %s", path, rr.Code, rr.Body.String())
		}
		var out struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		_ = json.Unmarshal(rr.Body.Bytes(), &out)
		body := rr.Body.String()
		if !strings.Contains(body, CodeRecipientDeprecated) || !strings.Contains(body, agent.URN) || !strings.Contains(body, "nothing was sent") {
			t.Fatalf("%s: refusal should name the code, the URN and that nothing was sent: %s", path, body)
		}
	}
	// Only the two sends made while the agent was active are in the mailbox.
	to, err := messaging.ParseURN(agent.URN)
	if err != nil {
		t.Fatal(err)
	}
	page, err := db.MessagingStore().List(ctx, to, store.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 {
		t.Fatalf("mailbox holds %d messages, want the 2 sent before deprecation", page.Total)
	}
}

func TestSendStillWorksForAddressesTheRegistryDoesNotHold(t *testing.T) {
	h, _, _ := deprecatedRecipientServer(t, nil)
	for name, to := range map[string]string{
		"unregistered agent": "msg://agent/local/worker",
		"user":               "msg://user/local/operator",
		"unknown mint":       "msg://agent/agent-mux/agt_neverexisted",
	} {
		if rr := postMessage(h, "/messages", to); rr.Code != http.StatusCreated {
			t.Errorf("%s: status %d: %s", name, rr.Code, rr.Body.String())
		}
	}
}

func TestSendIsNotRefusedWhenTheRegistryCannotAnswer(t *testing.T) {
	h, _, _ := deprecatedRecipientServer(t, func(svc *registry.Service) RegistryService { return failingLookup{svc} })
	if rr := postMessage(h, "/messages", "msg://agent/agent-mux/agt_anyone00001"); rr.Code != http.StatusCreated {
		t.Fatalf("a registry failure must not lose mail: status %d: %s", rr.Code, rr.Body.String())
	}
}
