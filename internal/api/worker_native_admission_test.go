package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

type nativeAdmissionReadService struct {
	*fakeLaunchService
	receipt *store.WorkerNativeAdmission
	err     error
}

func (s nativeAdmissionReadService) GetWorkerNativeAdmission(string) (*store.WorkerNativeAdmission, error) {
	return s.receipt, s.err
}

func TestSessionNativeAdmissionRead(t *testing.T) {
	for _, tc := range []string{"confirmed-local", "confirmed-remote", "unknown", "lookup-failed"} {
		t.Run(tc, func(t *testing.T) {
			row := &store.SessionRow{ID: "child", State: "running", Intent: "resume"}
			svc := nativeAdmissionReadService{fakeLaunchService: &fakeLaunchService{getRes: map[string]*store.SessionRow{"child": row}}, receipt: &store.WorkerNativeAdmission{Kind: "codex_same_worker_v1", SourceSessionID: "source", ReplacementSessionID: "child", Workroot: "/private/root"}}
			if tc == "unknown" {
				svc.receipt = nil
			}
			if tc == "lookup-failed" {
				svc.err = errors.New("private path")
			}
			req := httptest.NewRequest(http.MethodGet, "/sessions/child", nil)
			if tc == "confirmed-remote" {
				req = req.WithContext(identity.WithRemoteContext(req.Context()))
			}
			rr := httptest.NewRecorder()
			NewHandler(Deps{Service: svc}).ServeHTTP(rr, req)
			if tc == "lookup-failed" {
				if rr.Code != 500 || strings.Contains(rr.Body.String(), "private path") {
					t.Fatalf("unsafe failure: %d %s", rr.Code, rr.Body.String())
				}
				return
			}
			if rr.Code != 200 {
				t.Fatalf("%d %s", rr.Code, rr.Body.String())
			}
			var dto SessionDTO
			if err := json.Unmarshal(rr.Body.Bytes(), &dto); err != nil {
				t.Fatal(err)
			}
			if tc == "unknown" {
				if dto.NativeAdmission != nil || !strings.Contains(rr.Body.String(), `"native_admission":null`) {
					t.Fatalf("unknown promoted: %s", rr.Body.String())
				}
				return
			}
			if dto.NativeAdmission == nil || dto.NativeAdmission.ReplacementSessionID != "child" {
				t.Fatalf("receipt lost: %s", rr.Body.String())
			}
			if tc == "confirmed-remote" && (strings.Contains(rr.Body.String(), "/private/root") || !strings.HasPrefix(dto.NativeAdmission.Workroot, "tether-ref:workroot:")) {
				t.Fatalf("path leaked: %s", rr.Body.String())
			}
			if svc.receipt.Workroot != "/private/root" {
				t.Fatal("redaction mutated stored receipt")
			}
		})
	}
}
