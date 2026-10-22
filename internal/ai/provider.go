package ai

import (
	"context"
	"errors"
)

// ProviderKind identifies an LLM backend.
type ProviderKind string

const (
	ProviderGemini           ProviderKind = "gemini"
	ProviderAnthropic        ProviderKind = "anthropic"
	ProviderOpenAICompatible ProviderKind = "openai_compatible"
)

var (
	// ErrUnknownProvider is returned by the factory for an unrecognized kind.
	ErrUnknownProvider = errors.New("ai: unknown provider")
)

// ProviderConfig is a resolved, ready-to-use provider configuration.
// APIKey is plaintext and must only ever live in memory.
type ProviderConfig struct {
	Kind    ProviderKind
	APIKey  string
	BaseURL string // required for ProviderOpenAICompatible; ignored by native providers
	Model   string
}

// ToolDefinition represents a JSON schema for a tool.
type ToolDefinition struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Parameters  map[string]interface{} `json:"parameters"`
}

type ToolCall struct {
	ID        string                 `json:"id"`
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}

type ChatResponse struct {
	Content   string     `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// ChatTurn is one message in a conversation history.
type ChatTurn struct {
	Role       string     `json:"role"` // "user", "assistant", or "tool"
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"` // For tool responses (Anthropic/OpenAI key results by ID)
	Name       string     `json:"name,omitempty"`         // Tool name for a tool result (Gemini keys results by function name)
}

// LLMProvider is the capability set Trequila needs from an LLM backend.
// GenerateJSON returns JSON text already trimmed of markdown/prose (see CleanJSON);
// Chat returns the model's raw text.
type LLMProvider interface {
	ItineraryGenerator // GenerateItinerary + Close
	GenerateJSON(ctx context.Context, prompt string) (string, error)
	Chat(ctx context.Context, system string, history []ChatTurn, message string) (string, error)
	ChatWithTools(ctx context.Context, system string, history []ChatTurn, tools []ToolDefinition) (ChatResponse, error)
}
