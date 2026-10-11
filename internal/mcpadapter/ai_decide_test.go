package mcpadapter

import "testing"

func TestAITools_Decide(t *testing.T) {
	a := newAIAdapter(t, []string{ScopeAIInvoke})

	res := callAITool(t, a, "tether_ai_decide", map[string]any{
		"state":    map[string]any{"title": "fix login"},
		"purpose":  "task-kind",
		"on_error": "fail_open",
		"questions": []any{
			map[string]any{"key": "urgent", "type": "noul", "instructions": "Is it urgent?"},
		},
	})
	if res.IsError {
		t.Fatalf("decide error: %s", textOf(res))
	}
	body := parseToolJSON(t, res)
	if ok, _ := body["ok"].(bool); !ok {
		t.Fatalf("body = %v", body)
	}
}

func TestAITools_DecideValidatesInput(t *testing.T) {
	a := newAIAdapter(t, []string{ScopeAIInvoke})

	for name, args := range map[string]map[string]any{
		"no questions": {"state": "x"},
		"mixed forms":  {"request_json": `{"decision":{"state":"x"}}`, "state": "x"},
		"bad question": {"state": "x", "questions": []any{map[string]any{"key": "q", "type": "choice", "instructions": "i"}}},
	} {
		if res := callAITool(t, a, "tether_ai_decide", args); !res.IsError {
			t.Errorf("%s: expected an invalid_request error", name)
		}
	}
}

func TestAITools_DecideRequiresScope(t *testing.T) {
	a := newAIAdapter(t, nil)
	res := callAITool(t, a, "tether_ai_decide", map[string]any{
		"state":     "x",
		"questions": []any{map[string]any{"key": "q", "type": "noul", "instructions": "i"}},
	})
	if !res.IsError {
		t.Fatal("expected auth error")
	}
	if got := parseToolJSON(t, res)["code"]; got != "insufficient_scope" {
		t.Fatalf("code = %v", got)
	}
}
