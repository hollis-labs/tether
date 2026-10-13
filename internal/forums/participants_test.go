package forums

import (
	"context"
	"errors"
	"testing"

	f "github.com/hollis-labs/go-forums"
	"github.com/hollis-labs/tether/internal/store"
)

func TestParticipantLifecycle(t *testing.T) {
	s, db, operator := fixture(t)
	ctx := context.Background()
	pm := f.Caller{Provenance: f.Provenance{Principal: f.Principal{Kind: "agent", ID: "msg://agent/env/pm"}, Verified: true, SessionID: "pm-session", AgentURN: "msg://agent/env/pm", WorkstreamID: "work"}}
	invited := f.Caller{Provenance: f.Provenance{Principal: f.Principal{Kind: "agent", ID: "msg://agent/env/invited"}, Verified: true, SessionID: "invited-session", AgentURN: "msg://agent/env/invited", WorkstreamID: "work"}}
	stranger := f.Caller{Provenance: f.Provenance{Principal: f.Principal{Kind: "agent", ID: "stranger"}, Verified: true}}
	execute(t, s, operator, "forum_create", f.Request{ID: "portfolio", Namespace: "app/torque/forum/portfolio", Title: "Portfolio", OwnerType: pm.Kind, OwnerID: pm.ID})
	grant := func(c f.Caller, access string) {
		execute(t, s, operator, "participant_add", f.Request{ForumID: "portfolio", PrincipalKind: c.Kind, PrincipalID: c.ID, Access: access})
	}
	grant(pm, "post")
	execute(t, s, operator, "owner_set", f.Request{ForumID: "portfolio"})
	grant(invited, "read")
	denied := func(c f.Caller, op string, r f.Request) {
		t.Helper()
		_, err := s.Execute(ctx, c, op, r)
		if !errors.Is(err, f.ErrForbidden) {
			t.Fatalf("%s: expected forbidden, got %v", op, err)
		}
	}
	denied(invited, "topic_create", f.Request{ForumID: "portfolio", Title: "denied"})
	grant(invited, "post")
	topic := execute(t, s, pm, "topic_create", f.Request{ForumID: "portfolio", Title: "participant discussion"})["topic"].(map[string]any)["id"].(string)
	first := execute(t, s, pm, "post_create", f.Request{TopicID: topic, BodyMD: "participant hello"})["post"].(map[string]any)["id"].(string)
	execute(t, s, invited, "post_create", f.Request{TopicID: topic, ParentPostID: first, BodyMD: "participant reply"})
	for _, c := range []f.Caller{operator, pm, invited} {
		page := execute(t, s, c, "forum_list", f.Request{})
		if len(page["items"].([]any)) != 1 {
			t.Fatal("grant missing from discovery")
		}
		execute(t, s, c, "forum_get", f.Request{ForumID: "portfolio"})
		execute(t, s, c, "topic_list", f.Request{ForumID: "portfolio"})
		execute(t, s, c, "topic_get", f.Request{TopicID: topic})
		execute(t, s, c, "post_get", f.Request{PostID: first})
		execute(t, s, c, "forum_search", f.Request{ForumID: "portfolio", Query: "participant"})
	}
	page := execute(t, s, invited, "post_list", f.Request{TopicID: topic, Options: f.Options{Limit: 1}})
	cursor := page["next_cursor"].(string)
	execute(t, s, operator, "forum_create", f.Request{ID: "aaa", Namespace: "app/test/forum/aaa", Title: "First page"})
	execute(t, s, operator, "participant_add", f.Request{ForumID: "aaa", PrincipalKind: invited.Kind, PrincipalID: invited.ID, Access: "read"})
	discovery := execute(t, s, invited, "forum_list", f.Request{Options: f.Options{Limit: 1}})
	discoveryCursor := discovery["next_cursor"].(string)
	execute(t, s, operator, "participant_revoke", f.Request{ForumID: "portfolio", PrincipalKind: invited.Kind, PrincipalID: invited.ID})
	continuation := execute(t, s, invited, "forum_list", f.Request{Options: f.Options{Limit: 1, Cursor: discoveryCursor}})
	if len(continuation["items"].([]any)) != 0 {
		t.Fatal("discovery cursor retained revoked access")
	}
	execute(t, s, operator, "participant_revoke", f.Request{ForumID: "aaa", PrincipalKind: invited.Kind, PrincipalID: invited.ID})
	for _, c := range []f.Caller{invited, stranger} {
		if len(execute(t, s, c, "forum_list", f.Request{})["items"].([]any)) != 0 {
			t.Fatal("private forum discovered")
		}
		for _, entry := range []struct {
			op string
			r  f.Request
		}{
			{"forum_get", f.Request{ForumID: "portfolio"}}, {"topic_list", f.Request{ForumID: "portfolio"}},
			{"topic_get", f.Request{TopicID: topic}}, {"post_get", f.Request{PostID: first}},
			{"post_list", f.Request{TopicID: topic, Options: f.Options{Limit: 1, Cursor: cursor}}},
			{"post_list", f.Request{TopicID: topic, Options: f.Options{SinceSeq: 1}}},
			{"forum_search", f.Request{ForumID: "portfolio", Query: "participant"}},
			{"post_create", f.Request{TopicID: topic, BodyMD: "denied"}},
		} {
			denied(c, entry.op, entry.r)
		}
	}
	denied(pm, "participant_list", f.Request{ForumID: "portfolio"})
	denied(pm, "participant_add", f.Request{ForumID: "portfolio", PrincipalID: stranger.ID, PrincipalKind: stranger.Kind, Access: "post"})
	denied(pm, "participant_revoke", f.Request{ForumID: "portfolio", PrincipalID: operator.ID, PrincipalKind: operator.Kind})
	denied(pm, "owner_set", f.Request{ForumID: "portfolio"})
	for _, c := range []f.Caller{pm, invited} {
		refs, err := db.ListSessionRefs(c.SessionID, store.ListSessionRefsOptions{Kind: store.KindForumPost})
		if err != nil || len(refs) != 1 {
			t.Fatalf("atomic refs: %v %v", refs, err)
		}
	}
	// Revocation does not rewrite historical attribution.
	posts := execute(t, s, operator, "post_list", f.Request{TopicID: topic})["items"].([]any)
	provenance := posts[1].(map[string]any)["provenance"].(map[string]any)
	if provenance["id"] != invited.ID || provenance["session_id"] != invited.SessionID || provenance["workstream_id"] != "work" {
		t.Fatal(provenance)
	}
}
