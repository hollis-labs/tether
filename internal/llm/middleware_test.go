package llm

import (
	"context"
	"fmt"
	"reflect"
	"testing"
)

type testMiddleware struct {
	name   string
	events *[]string
}

func (m testMiddleware) Handle(ctx context.Context, req Request, next Handler) (Response, error) {
	*m.events = append(*m.events, "enter:"+m.name)
	resp, err := next(ctx, req)
	*m.events = append(*m.events, "exit:"+m.name)
	return resp, err
}

func TestBuildMiddlewareChainOrder(t *testing.T) {
	var events []string
	chain := BuildMiddlewareChain(
		func(_ context.Context, _ Request) (Response, error) {
			events = append(events, "handler")
			return Response{Provider: "test", Model: "demo"}, nil
		},
		[]Middleware{
			testMiddleware{name: "a", events: &events},
			testMiddleware{name: "b", events: &events},
		},
	)

	resp, err := chain(context.Background(), Request{Operation: OperationChat})
	if err != nil {
		t.Fatalf("chain returned err: %v", err)
	}
	if resp.Provider != "test" || resp.Model != "demo" {
		t.Fatalf("unexpected response: %+v", resp)
	}

	want := []string{
		"enter:a",
		"enter:b",
		"handler",
		"exit:b",
		"exit:a",
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

type errorMiddleware struct{}

func (errorMiddleware) Handle(_ context.Context, _ Request, _ Handler) (Response, error) {
	return Response{}, fmt.Errorf("blocked")
}

func TestBuildMiddlewareChainShortCircuits(t *testing.T) {
	called := false
	chain := BuildMiddlewareChain(
		func(_ context.Context, _ Request) (Response, error) {
			called = true
			return Response{}, nil
		},
		[]Middleware{errorMiddleware{}},
	)

	_, err := chain(context.Background(), Request{Operation: OperationChat})
	if err == nil || err.Error() != "blocked" {
		t.Fatalf("err = %v, want blocked", err)
	}
	if called {
		t.Fatal("terminal handler should not have been called")
	}
}
