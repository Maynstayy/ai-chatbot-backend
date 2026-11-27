package chatbot

import (
	"context"
	"encoding/json"
	"errors"
)

// Provider is the only thing the chatbot needs from an AI vendor: given the
// system prompt, the conversation so far and the approved tools, produce the
// assistant's reply, running any tool calls through Execute. Everything
// vendor-specific (SDK types, tool-call loop, retries) stays inside the
// implementation, so swapping vendors means adding one file.
type Provider interface {
	Reply(ctx context.Context, req ProviderRequest) (string, error)
}

// ProviderRequest is vendor-neutral.
type ProviderRequest struct {
	System  string
	History []Turn // oldest first; the last turn is the new user message
	Tools   []ToolSpec
	Execute ToolExecutor
}

// Turn is one stored chat message. Only plain text is kept between requests -
// tool calls and tool results live for a single request only.
type Turn struct {
	Role string `json:"role"` // "user" or "assistant"
	Text string `json:"text"`
}

// ToolSpec describes one approved backend tool as a JSON-schema object.
type ToolSpec struct {
	Name        string
	Description string
	Properties  map[string]any
	Required    []string
}

// ToolCall is the model asking to run one tool.
type ToolCall struct {
	Name  string
	Input json.RawMessage
}

// ToolExecutor runs an approved tool. The result is text (JSON) handed back to
// the model; isError marks a failed or rejected call.
type ToolExecutor func(ctx context.Context, call ToolCall) (result string, isError bool)

var (
	// ErrProviderRefused means the model declined to answer (safety refusal).
	ErrProviderRefused = errors.New("ai provider declined the request")
	// ErrProviderEmpty means the model finished without any text.
	ErrProviderEmpty = errors.New("ai provider returned no text")
)
