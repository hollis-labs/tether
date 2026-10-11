package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/llm"
)

const decideBody = `{"request":{"decision":{
  "state":"fix the login bug",
  "purpose":"task-kind",
  "on_error":"fail_open",
  "questions":[{"key":"urgent","type":"noul","instructions":"Is it urgent?"}]}}}`

func postDecide(h http.Handler, body string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/ai/decide", bytes.NewBufferString(body)))
	return rr
}

func TestAIDecideReturnsAnswers(t *testing.T) {
	t.Parallel()

	h := NewHandler(Deps{AI: stubAIService{resp: llm.Response{
		Provider: "jev",
		Model:    "jev-1.13.0",
		Usage:    llm.Usage{InputTokens: 12},
		Decision: &llm.DecisionResult{Answers: map[string]llm.DecisionAnswer{"urgent": {Type: llm.QuestionNoul, Noul: 0.8}}},
	}}})

	rr := postDecide(h, decideBody)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp ChatResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Response.Model != "jev-1.13.0" || resp.Response.Decision.Answers["urgent"].Noul != 0.8 {
		t.Fatalf("response = %+v", resp.Response)
	}
}

func TestAIDecideValidatesAndRejectsWrongOperation(t *testing.T) {
	t.Parallel()

	h := NewHandler(Deps{AI: stubAIService{}})
	for name, body := range map[string]string{
		"no decision":     `{"request":{}}`,
		"no questions":    `{"request":{"decision":{"state":"x","questions":[]}}}`,
		"bad on_error":    `{"request":{"decision":{"state":"x","on_error":"shrug","questions":[{"key":"q","type":"noul","instructions":"i"}]}}}`,
		"wrong operation": `{"request":{"operation":"chat","decision":{"state":"x","questions":[{"key":"q","type":"noul","instructions":"i"}]}}}`,
	} {
		if rr := postDecide(h, body); rr.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d body=%s", name, rr.Code, rr.Body.String())
		}
	}
}

func TestAIDecideTypedFailureCarriesKindAndMode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		kind       llm.DecisionErrorKind
		wantStatus int
		wantCode   string
	}{
		{llm.DecisionErrRateLimited, http.StatusTooManyRequests, CodeDecisionFailed},
		{llm.DecisionErrOverloaded, http.StatusServiceUnavailable, CodeDecisionFailed},
		{llm.DecisionErrCircuitOpen, http.StatusServiceUnavailable, CodeDecisionFailed},
		{llm.DecisionErrTimeout, http.StatusGatewayTimeout, CodeDecisionFailed},
		{llm.DecisionErrAuth, http.StatusBadGateway, CodeDecisionFailed},
		{llm.DecisionErrInvalid, http.StatusBadRequest, CodeInvalidRequest},
	}
	for _, tc := range cases {
		h := NewHandler(Deps{AI: stubAIService{err: &llm.DecisionError{Kind: tc.kind, Status: 429, OnError: llm.OnErrorFailOpen}}})
		rr := postDecide(h, decideBody)
		if rr.Code != tc.wantStatus {
			t.Errorf("%s: status = %d, want %d", tc.kind, rr.Code, tc.wantStatus)
		}
		var env ErrorResponse
		if err := json.NewDecoder(rr.Body).Decode(&env); err != nil {
			t.Fatal(err)
		}
		if env.Error.Code != tc.wantCode || env.Error.Details["kind"] != string(tc.kind) || env.Error.Details["on_error"] != "fail_open" {
			t.Errorf("%s: envelope = %+v", tc.kind, env.Error)
		}
		if strings.Contains(rr.Body.String(), "Bearer") {
			t.Errorf("%s: credentials in body", tc.kind)
		}
	}
}
