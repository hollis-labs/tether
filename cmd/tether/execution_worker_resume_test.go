package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/execpolicy"
)

func TestExecutionStatusShowsWorkerResume(t *testing.T) {
	for _, tc := range []struct {
		on   bool
		want string
	}{{false, "worker resume: off"}, {true, "worker resume: on"}} {
		var out bytes.Buffer
		printExecutionStatus(&out, execpolicy.Status{WorkerResume: tc.on})
		if !strings.Contains(out.String(), tc.want) {
			t.Fatalf("worker_resume=%v: output %q lacks %q", tc.on, out.String(), tc.want)
		}
	}
}
