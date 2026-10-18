package store

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"
)

// SessionCursor identifies an exclusive position in descending created_at, id order.
func SessionCursor(createdAt, id string) string {
	b, _ := json.Marshal([2]string{createdAt, id})
	return "v1." + base64.RawURLEncoding.EncodeToString(b)
}

// ParseSessionCursor also accepts legacy timestamp-only cursors.
func ParseSessionCursor(cursor string) (string, string, error) {
	if _, err := time.Parse(time.RFC3339Nano, cursor); err == nil {
		return cursor, "", nil
	}
	invalid := errors.New("invalid session cursor")
	if len(cursor) < 4 || cursor[:3] != "v1." {
		return "", "", invalid
	}
	b, err := base64.RawURLEncoding.DecodeString(cursor[3:])
	if err != nil {
		return "", "", invalid
	}
	var parts []string
	if json.Unmarshal(b, &parts) != nil || len(parts) != 2 || parts[1] == "" {
		return "", "", invalid
	}
	if _, err := time.Parse(time.RFC3339Nano, parts[0]); err != nil {
		return "", "", invalid
	}
	return parts[0], parts[1], nil
}
