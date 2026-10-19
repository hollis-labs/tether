package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
)

func TestMessageEventsGroupBodyRefResolvesAfterReadCursorAdvanced(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	reg := registry.NewService(registry.NewStorage(st.DB()))
	reg.SetDeliveryStore(st.DeliveryStore())
	owner, err := reg.Register(ctx, registry.KindAgent, registry.Profile{DisplayName: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := reg.Register(ctx, registry.KindAgent, registry.Profile{DisplayName: "reader"})
	if err != nil {
		t.Fatal(err)
	}
	group, err := reg.Register(ctx, registry.KindGroup, registry.Profile{DisplayName: "room", LastUpdatedBy: owner.URN})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reg.AddMember(ctx, group.URN, reader.URN, owner.URN, registry.MemberRoleMember); err != nil {
		t.Fatal(err)
	}
	posts := []registry.GroupMessage{}
	for _, text := range []string{`{"body":"first"}`, `{"body":"second"}`, `{"body":"third"}`} {
		post, err := reg.SendToGroup(ctx, group.URN, owner.URN, "notice", "", "application/json", json.RawMessage(text))
		if err != nil {
			t.Fatal(err)
		}
		posts = append(posts, post)
	}
	bus := events.NewBus(events.BusOptions{Persister: st}).(events.OutboxBus)
	if _, err = st.DrainMessageEvents(ctx, bus); err != nil {
		t.Fatal(err)
	}
	rows, err := st.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	refs := map[string]string{}
	for _, ev := range rows {
		if ev.Kind != "message.sent" {
			continue
		}
		var payload struct {
			ID  string `json:"message_id"`
			Ref string `json:"body_ref"`
		}
		if err = json.Unmarshal([]byte(ev.PayloadJSON), &payload); err != nil {
			t.Fatal(err)
		}
		refs[payload.ID] = payload.Ref
	}
	if err = reg.MarkRead(ctx, group.URN, reader.URN, posts[len(posts)-1].GroupSeq); err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(Deps{Registry: reg, Groups: reg, MessageStore: st.MessagingStore()})
	for _, post := range posts {
		ref := refs[post.ID]
		if ref == "" {
			t.Fatal("missing body ref")
		}
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, ref, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("body ref %s: status%d %s", ref, rr.Code, rr.Body.String())
		}
		var result listGroupMessagesResponse
		if err = json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Messages) != 1 || result.Messages[0].ID != post.ID || string(result.Messages[0].Payload) != string(post.Payload) {
			t.Fatalf("ref selected wrong post: %+v", result)
		}
	}
}
