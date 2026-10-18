package store

import "testing"

func TestSessionCursor(t *testing.T) {
	const stamp = "2026-04-19T10:00:00.123Z"
	cursor := SessionCursor(stamp, "id/with?punctuation")
	at, id, err := ParseSessionCursor(cursor)
	if err != nil || at != stamp || id != "id/with?punctuation" {
		t.Fatalf("roundtrip = %q, %q, %v", at, id, err)
	}
	at, id, err = ParseSessionCursor(stamp)
	if err != nil || at != stamp || id != "" {
		t.Fatalf("legacy = %q, %q, %v", at, id, err)
	}
	for _, bad := range []string{"", "bad", "v1.!", "v1.bnVsbA", SessionCursor(stamp, ""), SessionCursor("bad", "id")} {
		if _, _, err := ParseSessionCursor(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
