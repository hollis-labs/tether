package daemon

import (
	"net/http"
	"reflect"
	"testing"
)

// daemon.chat_wire off (no ChatWire handler) leaves the mount table as it was; on adds /chat/v1/ and nothing else.
func TestChatWireMountOnlyWithFlag(t *testing.T) {
	enabled := func(s *Server) []string {
		var out []string
		for _, m := range s.apiMounts() {
			if m.enabled {
				out = append(out, m.path)
			}
		}
		return out
	}
	off := &Server{Config: Config{TeamsEnabled: true}}
	on := &Server{Config: Config{TeamsEnabled: true, ChatWire: true}, ChatWire: http.NotFoundHandler()}
	offPaths, onPaths := enabled(off), enabled(on)
	for _, p := range offPaths {
		if p == "/chat/v1/" {
			t.Fatal("/chat/v1/ is mounted with the chat wire off")
		}
	}
	var added []string
	seen := map[string]bool{}
	for _, p := range offPaths {
		seen[p] = true
	}
	for _, p := range onPaths {
		if !seen[p] {
			added = append(added, p)
		}
	}
	if !reflect.DeepEqual(added, []string{"/chat/v1/"}) || len(onPaths) != len(offPaths)+1 {
		t.Fatalf("the chat wire added %v", added)
	}
}
