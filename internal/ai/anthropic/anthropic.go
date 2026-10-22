// Package anthropic implements ai.LLMProvider using the Anthropic Messages API.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sky-walkerx/trequila/internal/ai"
	"github.com/sky-walkerx/trequila/internal/types"
)

const (
	defaultBaseURL = "https://api.anthropic.com"
	defaultModel   = "claude-sonnet-4-6"
	apiVersion     = "2023-06-01"
	maxTokens      = 8192
)

// Provider talks to the Anthropic Messages API over HTTP.
type Provider struct {
	apiKey  string
	baseURL string
	model   string
	client  *http.Client
}

// NewProvider builds an Anthropic provider from a resolved ProviderConfig.
func NewProvider(_ context.Context, cfg ai.ProviderConfig) (*Provider, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("anthropic: API key is required")
	}
	base := cfg.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	model := cfg.Model
	if model == "" {
		model = defaultModel
	}
	return &Provider{
		apiKey:  cfg.APIKey,
		baseURL: strings.TrimRight(base, "/"),
		model:   model,
		client:  &http.Client{Timeout: 120 * time.Second},
	}, nil
}

type contentBlock struct {
	Type string `json:"type"` // "text" | "tool_use" | "tool_result"
	Text string `json:"text,omitempty"`

	// Tool use
	ID    string                 `json:"id,omitempty"`
	Name  string                 `json:"name,omitempty"`
	Input map[string]interface{} `json:"input,omitempty"`

	// Tool result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
}

type message struct {
	Role    string         `json:"role"` // "user" | "assistant"
	Content []contentBlock `json:"content"`
}

type request struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	System    string    `json:"system,omitempty"`
	Messages  []message `json:"messages"`
	Tools     []tool    `json:"tools,omitempty"`
}

type tool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	InputSchema map[string]interface{} `json:"input_schema"`
}

type response struct {
	Content []struct {
		Type  string                 `json:"type"`
		Text  string                 `json:"text,omitempty"`
		ID    string                 `json:"id,omitempty"`
		Name  string                 `json:"name,omitempty"`
		Input map[string]interface{} `json:"input,omitempty"`
	} `json:"content"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func (p *Provider) do(ctx context.Context, req request) (string, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	resp, err := ai.Retry(ctx, 3, func(ctx context.Context) (*http.Response, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/messages", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("x-api-key", p.apiKey)
		httpReq.Header.Set("anthropic-version", apiVersion)
		httpReq.Header.Set("content-type", "application/json")
		r, err := p.client.Do(httpReq)
		if err != nil {
			return nil, err
		}
		if r.StatusCode >= 400 {
			defer r.Body.Close()
			b, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
			return nil, fmt.Errorf("anthropic: status %d: %s", r.StatusCode, string(b))
		}
		return r, nil
	})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var out response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("anthropic: decode response: %w", err)
	}
	if out.Error != nil {
		return "", fmt.Errorf("anthropic: %s: %s", out.Error.Type, out.Error.Message)
	}
	var b strings.Builder
	for _, c := range out.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String(), nil
}

func (p *Provider) GenerateItinerary(ctx context.Context, trip types.TripRequest) ([]types.DayPlan, error) {
	prompt, err := ai.BuildItineraryPrompt(trip)
	if err != nil {
		return nil, err
	}
	raw, err := p.GenerateJSON(ctx, prompt)
	if err != nil {
		return nil, err
	}
	return ai.ParseItinerary(raw)
}

func (p *Provider) GenerateJSON(ctx context.Context, prompt string) (string, error) {
	out, err := p.do(ctx, request{
		Model:     p.model,
		MaxTokens: maxTokens,
		System:    "You are a precise assistant. Respond with ONLY valid JSON — no markdown fences, no prose.",
		Messages:  []message{{Role: "user", Content: []contentBlock{{Type: "text", Text: prompt}}}},
	})
	if err != nil {
		return "", err
	}
	return ai.CleanJSON(out), nil
}

func (p *Provider) Chat(ctx context.Context, system string, history []ai.ChatTurn, msg string) (string, error) {
	msgs := make([]message, 0, len(history)+1)
	for _, h := range history {
		role := h.Role
		if role == "model" {
			role = "assistant"
		}
		if role != "user" && role != "assistant" {
			role = "user"
		}
		msgs = append(msgs, message{Role: role, Content: []contentBlock{{Type: "text", Text: h.Content}}})
	}
	msgs = append(msgs, message{Role: "user", Content: []contentBlock{{Type: "text", Text: msg}}})
	return p.do(ctx, request{Model: p.model, MaxTokens: maxTokens, System: system, Messages: msgs})
}

func (p *Provider) ChatWithTools(ctx context.Context, system string, history []ai.ChatTurn, tools []ai.ToolDefinition) (ai.ChatResponse, error) {
	var msgs []message
	for _, h := range history {
		role := h.Role
		if role == "model" || role == "assistant" {
			role = "assistant"
		} else if role == "tool" {
			role = "user"
		} else {
			role = "user"
		}

		var blocks []contentBlock

		if h.Role == "tool" {
			if h.ToolCallID != "" {
				content := h.Content
				if content == "" {
					content = "{}"
				}
				blocks = append(blocks, contentBlock{
					Type:      "tool_result",
					ToolUseID: h.ToolCallID,
					Content:   content,
				})
			} else {
				for _, tc := range h.ToolCalls {
					resBytes, _ := json.Marshal(tc.Arguments)
					blocks = append(blocks, contentBlock{
						Type:      "tool_result",
						ToolUseID: tc.ID,
						Content:   string(resBytes),
					})
				}
			}
		} else {
			if h.Content != "" {
				blocks = append(blocks, contentBlock{Type: "text", Text: h.Content})
			}
			for _, tc := range h.ToolCalls {
				blocks = append(blocks, contentBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Name,
					Input: tc.Arguments,
				})
			}
		}

		msgs = append(msgs, message{Role: role, Content: blocks})
	}

	var anthropicTools []tool
	for _, t := range tools {
		anthropicTools = append(anthropicTools, tool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.Parameters,
		})
	}

	req := request{Model: p.model, MaxTokens: maxTokens, System: system, Messages: msgs, Tools: anthropicTools}

	body, err := json.Marshal(req)
	if err != nil {
		return ai.ChatResponse{}, err
	}
	resp, err := ai.Retry(ctx, 3, func(ctx context.Context) (*http.Response, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/messages", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("x-api-key", p.apiKey)
		httpReq.Header.Set("anthropic-version", apiVersion)
		httpReq.Header.Set("content-type", "application/json")
		r, err := p.client.Do(httpReq)
		if err != nil {
			return nil, err
		}
		if r.StatusCode >= 400 {
			defer r.Body.Close()
			b, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
			return nil, fmt.Errorf("anthropic: status %d: %s", r.StatusCode, string(b))
		}
		return r, nil
	})
	if err != nil {
		return ai.ChatResponse{}, err
	}
	defer resp.Body.Close()

	var out response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ai.ChatResponse{}, fmt.Errorf("anthropic: decode response: %w", err)
	}
	if out.Error != nil {
		return ai.ChatResponse{}, fmt.Errorf("anthropic: %s: %s", out.Error.Type, out.Error.Message)
	}

	var chatResp ai.ChatResponse
	for _, c := range out.Content {
		if c.Type == "text" {
			chatResp.Content += c.Text
		} else if c.Type == "tool_use" {
			chatResp.ToolCalls = append(chatResp.ToolCalls, ai.ToolCall{
				ID:        c.ID,
				Name:      c.Name,
				Arguments: c.Input,
			})
		}
	}
	return chatResp, nil
}

func (p *Provider) Close() error { return nil }

var _ ai.LLMProvider = (*Provider)(nil)
