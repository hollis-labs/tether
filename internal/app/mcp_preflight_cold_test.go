package app

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/testutil"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestPreflightDaemonMCPAllowsColdInitialization(t *testing.T) {
	root := testutil.SocketDir(t)
	address := "unix:" + filepath.Join(root, "daemon.sock")
	listener, err := net.Listen("unix", strings.TrimPrefix(address, "unix:"))
	if err != nil {
		t.Fatal(err)
	}
	daemon := mcp.NewServer(&mcp.Implementation{Name: "cold-preflight", Version: "1"}, &mcp.ServerOptions{SupportedProtocolVersions: []string{"2025-11-25"}})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return daemon }, nil)
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(strings.NewReader(string(body)))
			var request struct {
				Method string `json:"method"`
			}
			_ = json.Unmarshal(body, &request)
			if request.Method == "initialize" {
				// Deliberate cold-start duration crosses the old two-second setup bound.
				startup := time.NewTimer(2200 * time.Millisecond)
				defer startup.Stop()
				select {
				case <-startup.C:
				case <-r.Context().Done():
					return
				}
			}
		}
		handler.ServeHTTP(w, r)
	})}
	go func() { _ = server.Serve(listener) }()
	defer func() { _ = server.Close() }()
	cat := &config.Catalog{}
	cat.Global.Daemon.ListenAddr = address
	service := &Service{Catalog: cat}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if got, err := service.preflightDaemonMCP(ctx, "session"); err != nil || got != address {
		t.Fatalf("cold preflight failed: %s %v", got, err)
	}
}
