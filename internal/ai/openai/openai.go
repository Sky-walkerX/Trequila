package openai

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
	defaultModel = "gpt-4o"
)

// Provider talks to any OpenAI-compatible /chat/completions endpoint.
type Provider struct {
	apiKey  string
	baseURL string
	model   string
	client  *http.Client
}

// NewProvider builds an OpenAI-compatible provider from a resolved ProviderConfig.
func NewProvider(_ context.Context, cfg ai.ProviderConfig) (*Provider, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("openai: BaseURL is required for OpenAI-compatible endpoints")
	}
	// We don't strictly require APIKey because local models (Ollama/LM Studio)
	// might not need one, but if provided, we send it.
	model := cfg.Model
	if model == "" {
		model = defaultModel
	}
	return &Provider{
		apiKey:  cfg.APIKey,
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		model:   model,
		client:  &http.Client{Timeout: 120 * time.Second},
	}, nil
}

type message struct {
	Role       string      `json:"role"`
	Content    interface{} `json:"content"`
	ToolCalls  []toolCall  `json:"tool_calls,omitempty"`
	ToolCallID string      `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function functionCall `json:"function"`
}

type functionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type contentPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type responseFormat struct {
	Type string `json:"type"`
}

type request struct {
	Model          string          `json:"model"`
	Messages       []message       `json:"messages"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
	Tools          []tool          `json:"tools,omitempty"`
}

type tool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string                 `json:"name"`
		Description string                 `json:"description,omitempty"`
		Parameters  map[string]interface{} `json:"parameters,omitempty"`
	} `json:"function"`
}

type response struct {
	Choices []struct {
		Message struct {
			Content   string     `json:"content"`
			ToolCalls []toolCall `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

func (p *Provider) do(ctx context.Context, req request) (string, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	resp, err := ai.Retry(ctx, 3, func(ctx context.Context) (*http.Response, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		if p.apiKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		r, err := p.client.Do(httpReq)
		if err != nil {
			return nil, err
		}
		if r.StatusCode >= 400 {
			defer r.Body.Close()
			b, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
			return nil, fmt.Errorf("openai: status %d: %s", r.StatusCode, string(b))
		}
		return r, nil
	})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var out response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("openai: decode response: %w", err)
	}
	if out.Error != nil {
		return "", fmt.Errorf("openai: %s: %s", out.Error.Type, out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("openai: no choices returned")
	}
	return out.Choices[0].Message.Content, nil
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
		Model: p.model,
		Messages: []message{
			{Role: "system", Content: "You are a precise assistant. Respond with ONLY valid JSON — no markdown fences, no prose."},
			{Role: "user", Content: prompt},
		},
		ResponseFormat: &responseFormat{Type: "json_object"},
	})
	if err != nil {
		// Fallback for providers that don't support response_format: json_object
		if strings.Contains(err.Error(), "response_format") || strings.Contains(err.Error(), "json_object") {
			out, err = p.do(ctx, request{
				Model: p.model,
				Messages: []message{
					{Role: "system", Content: "You are a precise assistant. Respond with ONLY valid JSON — no markdown fences, no prose."},
					{Role: "user", Content: prompt},
				},
			})
			if err != nil {
				return "", err
			}
		} else {
			return "", err
		}
	}
	return ai.CleanJSON(out), nil
}

func (p *Provider) Chat(ctx context.Context, system string, history []ai.ChatTurn, msg string) (string, error) {
	var msgs []message
	if system != "" {
		msgs = append(msgs, message{Role: "system", Content: system})
	}
	for _, h := range history {
		role := h.Role
		if role == "model" {
			role = "assistant"
		}
		msgs = append(msgs, message{Role: role, Content: h.Content})
	}
	msgs = append(msgs, message{Role: "user", Content: msg})
	return p.do(ctx, request{Model: p.model, Messages: msgs})
}

func (p *Provider) ChatWithTools(ctx context.Context, system string, history []ai.ChatTurn, tools []ai.ToolDefinition) (ai.ChatResponse, error) {
	var msgs []message
	if system != "" {
		msgs = append(msgs, message{Role: "system", Content: system})
	}
	for _, h := range history {
		role := h.Role
		if role == "model" {
			role = "assistant"
		}

		msg := message{Role: role}
		if h.Content != "" {
			msg.Content = h.Content
		}

		if role == "tool" {
			if h.ToolCallID != "" {
				msg.ToolCallID = h.ToolCallID
				msg.Content = h.Content
				if msg.Content == nil || msg.Content == "" {
					msg.Content = "{}"
				}
				msgs = append(msgs, msg)
			} else {
				for _, tc := range h.ToolCalls {
					resBytes, _ := json.Marshal(tc.Arguments)
					m := message{
						Role:       role,
						ToolCallID: tc.ID,
						Content:    string(resBytes),
					}
					msgs = append(msgs, m)
				}
			}
			continue
		}

		for _, tc := range h.ToolCalls {
			argsBytes, _ := json.Marshal(tc.Arguments)
			msg.ToolCalls = append(msg.ToolCalls, toolCall{
				ID:   tc.ID,
				Type: "function",
				Function: functionCall{
					Name:      tc.Name,
					Arguments: string(argsBytes),
				},
			})
		}
		msgs = append(msgs, msg)
	}

	var openaiTools []tool
	for _, t := range tools {
		openaiTools = append(openaiTools, tool{
			Type: "function",
			Function: struct {
				Name        string                 `json:"name"`
				Description string                 `json:"description,omitempty"`
				Parameters  map[string]interface{} `json:"parameters,omitempty"`
			}{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}

	req := request{Model: p.model, Messages: msgs, Tools: openaiTools}

	body, err := json.Marshal(req)
	if err != nil {
		return ai.ChatResponse{}, err
	}
	resp, err := ai.Retry(ctx, 3, func(ctx context.Context) (*http.Response, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		if p.apiKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		r, err := p.client.Do(httpReq)
		if err != nil {
			return nil, err
		}
		if r.StatusCode >= 400 {
			defer r.Body.Close()
			b, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
			return nil, fmt.Errorf("openai: status %d: %s", r.StatusCode, string(b))
		}
		return r, nil
	})
	if err != nil {
		return ai.ChatResponse{}, err
	}
	defer resp.Body.Close()

	var out response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ai.ChatResponse{}, fmt.Errorf("openai: decode response: %w", err)
	}
	if out.Error != nil {
		return ai.ChatResponse{}, fmt.Errorf("openai: %s: %s", out.Error.Type, out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return ai.ChatResponse{}, fmt.Errorf("openai: no choices returned")
	}

	choice := out.Choices[0].Message
	chatResp := ai.ChatResponse{
		Content: choice.Content,
	}

	for _, tc := range choice.ToolCalls {
		var args map[string]interface{}
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		chatResp.ToolCalls = append(chatResp.ToolCalls, ai.ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: args,
		})
	}

	return chatResp, nil
}

func (p *Provider) Close() error { return nil }

var _ ai.LLMProvider = (*Provider)(nil)
