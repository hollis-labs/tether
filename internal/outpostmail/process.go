package outpostmail

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Same ps identity witness as the session reaper, plus uid ownership.
func ProcessIdentity(pid int) (string, int, error) {
	if pid <= 0 {
		return "", 0, fmt.Errorf("invalid pid")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "uid=", "-o", "lstart=")
	cmd.Env = append(os.Environ(), "TZ=UTC", "LC_ALL=C")
	b, err := cmd.Output()
	if err != nil {
		return "", 0, err
	}
	parts := strings.Fields(string(b))
	if len(parts) != 6 {
		return "", 0, fmt.Errorf("process identity unavailable")
	}
	uid, err := strconv.Atoi(parts[0])
	if err != nil {
		return "", 0, err
	}
	started, err := time.Parse("Mon Jan _2 15:04:05 2006", strings.Join(parts[1:], " "))
	return started.UTC().Format(time.RFC3339), uid, err
}
