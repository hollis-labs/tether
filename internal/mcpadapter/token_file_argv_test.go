package mcpadapter

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
)

const argvTestCredential = "fake-credential-for-argv-test-only"

func TestTokenFileUpstreamHelper(t *testing.T) {
	if os.Getenv("TETHER_TOKEN_FILE_ARGV_FIXTURE") != "1" {
		return
	}
	args := os.Args
	if len(args) < 3 || args[len(args)-2] != "--token-file" {
		os.Exit(3)
	}
	value, err := os.ReadFile(args[len(args)-1])
	if err != nil || strings.TrimSpace(string(value)) != argvTestCredential {
		os.Exit(4)
	}
	// Exercise the supervisor's stderr redaction too, without forwarding
	// this captured fixture output to the test transcript.
	_, _ = fmt.Fprintln(os.Stderr, argvTestCredential)
	_ = json.NewEncoder(os.Stdout).Encode(args)
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

func TestTokenFileNeverReachesActualUpstreamArgv(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "credential")
	if err := os.WriteFile(file, []byte(argvTestCredential+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "mcp-servers"), 0700); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Load authored catalog source as a real proxy does; listing must retain
	// the path, and the process spawn must validate the file again.
	yaml := fmt.Sprintf("id: fixture\ntransport: stdio\ncommand: %q\nargs: [\"-test.run=^TestTokenFileUpstreamHelper$\", \"--\"]\ntoken_file: %q\nenv:\n  TETHER_TOKEN_FILE_ARGV_FIXTURE: '1'\n", exe, file)
	if err := os.WriteFile(filepath.Join(root, "mcp-servers", "fixture.yaml"), []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	entries, err := config.LoadMCPServerCatalog(root)
	if err != nil || len(entries) != 1 {
		t.Fatalf("catalog load: %v", err)
	}
	u, transport, err := spawnStdioUpstream(entries[0])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = transport.Reader.Close(); _ = transport.Writer.Close() }()
	line, err := bufio.NewReader(transport.Reader).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var observed []string
	if err := json.Unmarshal(line, &observed); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(observed, "\x00"), argvTestCredential) {
		t.Fatal("credential leaked into child-observed argv")
	}
	if observed[len(observed)-2] != "--token-file" || observed[len(observed)-1] != file {
		t.Fatal("child did not receive credential path")
	}
	if runtime.GOOS == "linux" {
		cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", u.cmd.Process.Pid))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(cmdline, []byte(argvTestCredential)) {
			t.Fatal("credential leaked into kernel argv")
		}
		if !bytes.Contains(cmdline, []byte(file)) {
			t.Fatal("kernel argv lacks file path")
		}
	}
	_ = transport.Writer.Close()
	if err := u.cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(u.stderr.String(), argvTestCredential) {
		t.Fatal("credential leaked into captured stderr")
	}
	// Re-check the file at each spawn, including reconnects.
	if err := os.Chmod(file, 0644); err != nil {
		t.Fatal(err)
	}
	if child, _, err := spawnStdioUpstream(entries[0]); err == nil || child != nil {
		t.Fatal("permissive file spawned upstream")
	}
}
