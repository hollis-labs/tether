//go:build linux

package app

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/shimhost"
	"golang.org/x/sys/unix"
)

func fixtureProcessIdentity(pid int) (parent int, start uint64, state string, err error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, 0, "", err
	}
	i := strings.LastIndex(string(data), ")")
	if i < 0 {
		return 0, 0, "", fmt.Errorf("invalid process stat")
	}
	fields := strings.Fields(string(data)[i+2:])
	if len(fields) < 20 {
		return 0, 0, "", fmt.Errorf("truncated process stat")
	}
	parent, err = strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, "", err
	}
	start, err = strconv.ParseUint(fields[19], 10, 64)
	return parent, start, fields[0], err
}

// Fallback cleanup uses an owned process handle and recorded start time. The
// provider helper's parent-death signal ends it when this host is terminated.
func terminateOwnedShimFixture(t *testing.T, r shimhost.Receipt) {
	t.Helper()
	fd, err := unix.PidfdOpen(r.HostPID, 0)
	if errors.Is(err, syscall.ESRCH) {
		return
	}
	if err != nil {
		t.Errorf("fixture pidfd: %v", err)
		return
	}
	defer func() { _ = unix.Close(fd) }()
	parent, start, _, err := fixtureProcessIdentity(r.HostPID)
	if err != nil || parent != os.Getpid() || r.HostStartTime == 0 || start != r.HostStartTime {
		t.Errorf("fixture cleanup identity refused: %v", err)
		return
	}
	if err := unix.PidfdSendSignal(fd, syscall.SIGKILL, nil, 0); err != nil && !errors.Is(err, syscall.ESRCH) {
		t.Errorf("fixture cleanup signal: %v", err)
	}
}

func TestShimFixtureProviderDiesWithParent(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestShimLaunchProcess$", "--", "guardian")
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	scan := bufio.NewScanner(output)
	ready := make(chan bool, 1)
	go func() { ready <- scan.Scan() }()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatalf("guardian did not report ready provider: %v", scan.Err())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("guardian readiness deadline exceeded")
	}
	pid, err := strconv.Atoi(scan.Text())
	if err != nil || pid <= 0 {
		t.Fatalf("invalid provider pid: %v", err)
	}
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	parent, start, _, err := fixtureProcessIdentity(pid)
	if err != nil || parent != cmd.Process.Pid || start == 0 {
		t.Fatalf("provider ownership refused: %v", err)
	}
	// Kill only the exec.Cmd-owned parent. A ready descendant must then die,
	// even if normal Go test cleanup cannot run in that parent.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	n, err := unix.Poll(poll, 1000)
	if err != nil || n != 1 || poll[0].Revents&unix.POLLIN == 0 {
		_ = unix.PidfdSendSignal(fd, syscall.SIGKILL, nil, 0)
		t.Fatalf("provider survived test parent: %v", err)
	}
}
