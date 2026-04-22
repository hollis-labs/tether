package mcpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// ─── helpers ──────────────────────────────────────────────────────────────────

// recordingMiddleware records whether it was called and in which order.
type recordingMiddleware struct {
	id      int
	called  *[]int
	callCtx *[]context.Context
}

func (m *recordingMiddleware) Handle(ctx context.Context, req mcp.CallToolRequest, next ToolCallHandler) (*mcp.CallToolResult, error) {
	*m.called = append(*m.called, m.id)
	return next(ctx, req)
}

// shortCircuitMiddleware never calls next.
type shortCircuitMiddleware struct {
	result *mcp.CallToolResult
}

func (m *shortCircuitMiddleware) Handle(_ context.Context, _ mcp.CallToolRequest, _ ToolCallHandler) (*mcp.CallToolResult, error) {
	return m.result, nil
}

// errorMiddleware always returns an error.
type errorMiddleware struct{}

func (m *errorMiddleware) Handle(_ context.Context, _ mcp.CallToolRequest, _ ToolCallHandler) (*mcp.CallToolResult, error) {
	return nil, errors.New("middleware error")
}

func terminalOK(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultText("terminal"), nil
}

// ─── chain tests ──────────────────────────────────────────────────────────────

func TestBuildMiddlewareChain_Empty(t *testing.T) {
	chain := buildMiddlewareChain(terminalOK, nil)
	result, err := chain(context.Background(), callReq("tool"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Error("expected non-error result")
	}
}

func TestBuildMiddlewareChain_Single(t *testing.T) {
	called := make([]int, 0, 1)
	mw := &recordingMiddleware{id: 1, called: &called}

	chain := buildMiddlewareChain(terminalOK, []ToolCallMiddleware{mw})
	result, err := chain(context.Background(), callReq("tool"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Error("expected non-error result")
	}
	if len(called) != 1 || called[0] != 1 {
		t.Errorf("called = %v, want [1]", called)
	}
}

func TestBuildMiddlewareChain_Order(t *testing.T) {
	// Middleware should execute in slice order: [0] outermost (first in, last out).
	called := make([]int, 0, 3)
	mw1 := &recordingMiddleware{id: 1, called: &called}
	mw2 := &recordingMiddleware{id: 2, called: &called}
	mw3 := &recordingMiddleware{id: 3, called: &called}

	chain := buildMiddlewareChain(terminalOK, []ToolCallMiddleware{mw1, mw2, mw3})
	_, err := chain(context.Background(), callReq("tool"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(called) != 3 {
		t.Fatalf("expected 3 calls, got %d: %v", len(called), called)
	}
	// First middleware in slice should execute first.
	if called[0] != 1 || called[1] != 2 || called[2] != 3 {
		t.Errorf("execution order = %v, want [1 2 3]", called)
	}
}

func TestBuildMiddlewareChain_ShortCircuit(t *testing.T) {
	// Short-circuit middleware must prevent terminal from being called.
	terminalCalled := false
	terminal := ToolCallHandler(func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		terminalCalled = true
		return mcp.NewToolResultText("terminal"), nil
	})

	mw := &shortCircuitMiddleware{result: mcp.NewToolResultText("short-circuited")}
	chain := buildMiddlewareChain(terminal, []ToolCallMiddleware{mw})
	result, err := chain(context.Background(), callReq("tool"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if terminalCalled {
		t.Error("terminal should not have been called")
	}
	if len(result.Content) == 0 {
		t.Fatal("expected result content")
	}
}

func TestBuildMiddlewareChain_ErrorPropagated(t *testing.T) {
	chain := buildMiddlewareChain(terminalOK, []ToolCallMiddleware{&errorMiddleware{}})
	_, err := chain(context.Background(), callReq("tool"))
	if err == nil {
		t.Error("expected error from errorMiddleware")
	}
}

// ─── ArgsSchemaFP tests ───────────────────────────────────────────────────────

func TestArgsSchemaFP_Empty(t *testing.T) {
	fp := ArgsSchemaFP(nil)
	if fp != "00000000" {
		t.Errorf("nil args FP = %q, want 00000000", fp)
	}
	fp = ArgsSchemaFP(json.RawMessage(""))
	if fp != "00000000" {
		t.Errorf("empty args FP = %q, want 00000000", fp)
	}
}

func TestArgsSchemaFP_SameKeysAreStable(t *testing.T) {
	args1, _ := json.Marshal(map[string]any{"b": 2, "a": 1})
	args2, _ := json.Marshal(map[string]any{"a": "x", "b": "y"})

	fp1 := ArgsSchemaFP(args1)
	fp2 := ArgsSchemaFP(args2)
	if fp1 != fp2 {
		t.Errorf("same keys different values: fp1=%q fp2=%q (should be equal)", fp1, fp2)
	}
}

func TestArgsSchemaFP_DifferentKeysAreDifferent(t *testing.T) {
	args1, _ := json.Marshal(map[string]any{"a": 1})
	args2, _ := json.Marshal(map[string]any{"b": 1})

	fp1 := ArgsSchemaFP(args1)
	fp2 := ArgsSchemaFP(args2)
	if fp1 == fp2 {
		t.Errorf("different keys produced same FP: %q", fp1)
	}
}

func TestArgsSchemaFP_Length(t *testing.T) {
	args, _ := json.Marshal(map[string]any{"key": "value"})
	fp := ArgsSchemaFP(args)
	if len(fp) != 8 {
		t.Errorf("FP length = %d, want 8", len(fp))
	}
}

func TestArgsSchemaFP_NotAnObject(t *testing.T) {
	fp := ArgsSchemaFP(json.RawMessage(`"not an object"`))
	if fp != "00000000" {
		t.Errorf("non-object FP = %q, want 00000000", fp)
	}
}
