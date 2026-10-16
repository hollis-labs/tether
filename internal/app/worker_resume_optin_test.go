package app

import (
	"errors"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workerhub"
)

// catalog.defaults.worker_resume is the opt-in: off (the default) is today's refusal, on gets past it to the
// worker checks (connected, advertises worker-resume), which still fail closed.
func TestWorkerResumeOptInFromCatalog(t *testing.T) {
	svc := bindingHarness(t)
	svc.Catalog = &config.Catalog{Global: config.Global{Version: "test"}}
	parent := &store.SessionRow{ID: "wparent", WorkerID: "w1", ProviderID: "claude"}

	if svc.workerResumeEnabled() {
		t.Fatal("worker resume must default to off")
	}
	err := svc.checkWorkerResume(parent, api.ResumeOptions{})
	if !errors.Is(err, session.ErrRecoveryConflict) || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("off: err = %v, want the not-enabled refusal", err)
	}
	st, err := svc.ExecutionStatus()
	if err != nil || st.WorkerResume {
		t.Fatalf("off: status worker_resume = %v, err = %v", st.WorkerResume, err)
	}

	svc.Catalog.Global.Catalog.Defaults.WorkerResume = true
	if !svc.workerResumeEnabled() {
		t.Fatal("catalog.defaults.worker_resume = true must enable it")
	}
	err = svc.checkWorkerResume(parent, api.ResumeOptions{})
	if !errors.Is(err, workerhub.ErrChannelUnavailable) || strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("on: err = %v, want to pass the opt-in and stop at the worker channel check", err)
	}
	if st, err = svc.ExecutionStatus(); err != nil || !st.WorkerResume {
		t.Fatalf("on: status worker_resume = %v, err = %v", st.WorkerResume, err)
	}

	svc.Catalog.Global.Catalog.Defaults.WorkerResume = false
	svc.WorkerResume = true
	if !svc.workerResumeEnabled() {
		t.Fatal("the Service field must still enable it for embedders")
	}
}
