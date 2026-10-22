package store

import "context"

type inboxUnreadKey struct{}

// WithUnreadOnlyInbox asks Inbox for the messages the recipient has not read yet.
// The pull a live recipient session makes for its own mailbox passes it
// (CW-20261021-0029): Inbox returns every undelivered message, read or not, and that
// pull consumes whatever it returns, so a message the agent had already handled came
// back as new and was consumed again. Messages that are read but still undelivered are
// not returned; the pull settles them instead (see deliveryBackedStore.Inbox). Operator
// and CLI pulls do not set it and are unchanged.
func WithUnreadOnlyInbox(ctx context.Context) context.Context {
	return context.WithValue(ctx, inboxUnreadKey{}, true)
}

func unreadOnlyInbox(ctx context.Context) bool {
	v, _ := ctx.Value(inboxUnreadKey{}).(bool)
	return v
}

// InboxUnreadOnly reports whether ctx carries WithUnreadOnlyInbox.
func InboxUnreadOnly(ctx context.Context) bool { return unreadOnlyInbox(ctx) }
