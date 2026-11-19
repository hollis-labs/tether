package workerhub

import "context"

// SetLookupHeld replaces how h looks up a session a reconnecting worker holds.
func SetLookupHeld(h *Hub, f func(ctx context.Context, sessionID, workerID string) (string, int64, error)) {
	h.lookupHeld = f
}
