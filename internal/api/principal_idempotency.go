package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/hollis-labs/tether/internal/identity"
)

// Local callers retain the existing namespace. Remote keys bind to the actual
// verified device, never a label, caller-selected from/as, address or header.
// Framed JSON prevents concatenation ambiguity; hashing bounds arbitrary keys
// without persisting the credential or exposing it to the launch service.
func principalIdempotencyKey(ctx context.Context, key string) (string, error) {
	if !identity.IsRemote(ctx) {
		return key, nil
	}
	p, ok := identity.FromContext(ctx)
	if !ok || p.Kind != "device" || p.ID == "" || p.ID == identity.OperatorID {
		return "", errors.New("verified device required")
	}
	if key == "" {
		return "", nil
	}
	data, _ := json.Marshal([2]string{p.ID, key})
	sum := sha256.Sum256(data)
	return "device_v1_" + hex.EncodeToString(sum[:]), nil
}
