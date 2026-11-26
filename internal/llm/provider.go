// Package llm defines the Provider interface. A provider is a pure function
// (prompt, schema) -> JSON: it has no tools and takes no action. Deciding
// what to do with the output is the job of Go code elsewhere.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Request is one structured-output call.
type Request struct {
	// Task selects the model settings (triage, draft, ask).
	Task string
	// System is the system prompt (static rules + profile.md).
	System string
	// Prompt is the user message, including untrusted email content wrapped
	// in explicit tags. Providers must pass it out of band (stdin, body),
	// never on a command line.
	Prompt string
	// Schema is the JSON Schema the output must follow.
	Schema json.RawMessage
}

// Usage reports token counts for cost tracking.
type Usage struct {
	InputTokens       int64
	OutputTokens      int64
	CacheReadTokens   int64
	CacheCreateTokens int64
}

// Response is a successful call.
type Response struct {
	Output   json.RawMessage
	Model    string
	Usage    Usage
	CostUSD  float64
	Duration time.Duration
}

// Provider produces schema-constrained JSON.
type Provider interface {
	Name() string
	Complete(ctx context.Context, req Request) (*Response, error)
}

// ErrToolsExposed means the provider would have given the model access to a
// tool or MCP server. It is fatal: mailpilot refuses to run in that state.
var ErrToolsExposed = errors.New("llm: provider exposes tools to the model")

// ErrNoStructuredOutput means the call finished without schema output.
var ErrNoStructuredOutput = errors.New("llm: no structured output")
