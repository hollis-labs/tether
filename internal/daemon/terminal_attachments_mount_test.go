package daemon

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/hollis-labs/tether/internal/terminalattach"
)

type nopTerminalHost struct{}

func (nopTerminalHost) Resolve(context.Context, string) (terminalattach.Binding, error) {
	return terminalattach.Binding{}, terminalattach.ErrNotFound
}
func (nopTerminalHost) Attach(context.Context, terminalattach.Binding, terminalattach.Role) (string, error) {
	return "", terminalattach.ErrNotFound
}
func (nopTerminalHost) Detach(context.Context, terminalattach.Binding, string, terminalattach.Role) error {
	return nil
}
func (nopTerminalHost) Claim(context.Context, terminalattach.Binding, string, bool) (terminalattach.Lease, error) {
	return terminalattach.Lease{}, terminalattach.ErrNotFound
}
func (nopTerminalHost) Renew(context.Context, terminalattach.Lease) (terminalattach.Lease, error) {
	return terminalattach.Lease{}, terminalattach.ErrNotFound
}
func (nopTerminalHost) Observe(context.Context, terminalattach.Binding, io.Writer, int64, func(terminalattach.Window) error) error {
	return terminalattach.ErrNotFound
}
func (nopTerminalHost) Input(context.Context, terminalattach.Lease, []byte) error { return nil }
func (nopTerminalHost) Resize(context.Context, terminalattach.Lease, uint16, uint16) error {
	return nil
}

// daemon.terminal_attachments off (no TerminalHost) leaves the mount table as it was; on adds exactly the two
// /terminal-attachments paths (CW-20261118-0012).
func TestTerminalAttachmentsMountOnlyWithFlag(t *testing.T) {
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
	on := &Server{Config: Config{TeamsEnabled: true, TerminalAttachments: true}, TerminalHost: nopTerminalHost{}}
	seen := map[string]bool{}
	for _, p := range enabled(off) {
		if p == "/terminal-attachments" || p == "/terminal-attachments/" {
			t.Fatalf("%s is mounted with terminal attachments off", p)
		}
		seen[p] = true
	}
	var added []string
	for _, p := range enabled(on) {
		if !seen[p] {
			added = append(added, p)
		}
	}
	if !reflect.DeepEqual(added, []string{"/terminal-attachments", "/terminal-attachments/"}) {
		t.Fatalf("terminal attachments added %v", added)
	}
}

// Exercise the production router as well as its mount list.
func TestTerminalAttachmentsRouteDisabledByDefault(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "off", true: "on"}[enabled], func(t *testing.T) {
			s := &Server{Catalog: stubCatalogLoader{}, Config: Config{TerminalAttachments: enabled}}
			want := http.StatusNotFound
			if enabled {
				s.TerminalHost = nopTerminalHost{}
				want = http.StatusMethodNotAllowed
			}
			for _, path := range []string{"/terminal-attachments", "/terminal-attachments/example"} {
				w := httptest.NewRecorder()
				s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
				if w.Code != want {
					t.Fatalf("GET %s = %d, want %d: %s", path, w.Code, want, w.Body.String())
				}
			}
		})
	}
}
