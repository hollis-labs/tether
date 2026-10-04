package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

type shimStopFault struct{ code string }

func (e shimStopFault) Error() string     { return e.code + ": host outcome retained" }
func (e shimStopFault) ErrorCode() string { return e.code }

func TestHandleStopSessionPreservesTypedShimFailure(t *testing.T) {
	for _, code := range []string{"outcome_unknown", "identity_mismatch", "journal_mismatch", "unauthorized"} {
		svc := &fakeLaunchService{stopErr: shimStopFault{code: code}}
		rr := httptest.NewRecorder()
		newTestHandler(svc).ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/sessions/s1/stop", nil))
		if rr.Code != http.StatusConflict || decodeErr(t, rr).Error.Code != code {
			t.Fatalf("typed stop code lost: %d %s", rr.Code, rr.Body.String())
		}
	}
}
