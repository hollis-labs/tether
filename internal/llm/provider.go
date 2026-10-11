package llm

import "context"

// ChatProvider executes a normalized chat request against one concrete
// provider/runtime that has already been selected by the router.
type ChatProvider interface {
	Chat(ctx context.Context, req Request, route RouteDecision) (Response, error)
}

// EmbeddingProvider executes a normalized embedding request against one
// concrete provider/runtime that has already been selected by the router.
type EmbeddingProvider interface {
	Embed(ctx context.Context, req Request, route RouteDecision) (Response, error)
}

// DecisionProvider executes a normalized structured-judgment request against
// one concrete provider selected by the decision planner. Chat and embedding
// routes never reach a DecisionProvider.
type DecisionProvider interface {
	Decide(ctx context.Context, req Request, route RouteDecision) (Response, error)
}

// StreamChatProvider is the optional streaming extension for a chat provider.
// Implementations emit normalized stream events through emit and return the
// final accumulated response once the vendor stream completes.
type StreamChatProvider interface {
	StreamChat(ctx context.Context, req Request, route RouteDecision, emit func(StreamEvent) error) (Response, error)
}
