// Package gemini implements ai.LLMProvider using Google Gemini.
package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/generative-ai-go/genai"
	"github.com/sky-walkerx/trequila/internal/ai"
	"github.com/sky-walkerx/trequila/internal/types"
	"google.golang.org/api/option"
)

const defaultModel = "gemini-2.5-flash"

// Generator is a Gemini-backed LLMProvider. Models are created per call so the
// same client can serve JSON, chat, and vision requests.
type Generator struct {
	client    *genai.Client
	modelName string
	timeout   time.Duration
}

// NewProvider builds a Gemini provider from a resolved ProviderConfig.
func NewProvider(ctx context.Context, cfg ai.ProviderConfig) (*Generator, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("gemini: API key is required")
	}
	client, err := genai.NewClient(ctx, option.WithAPIKey(cfg.APIKey))
	if err != nil {
		return nil, fmt.Errorf("gemini: failed to create client: %w", err)
	}
	name := cfg.Model
	if name == "" {
		name = defaultModel
	}
	return &Generator{client: client, modelName: name, timeout: 90 * time.Second}, nil
}

func (g *Generator) jsonModel() *genai.GenerativeModel {
	m := g.client.GenerativeModel(g.modelName)
	m.SetTemperature(0.7)
	m.SetMaxOutputTokens(16384)
	m.ResponseMIMEType = "application/json"
	return m
}

// GenerateItinerary builds the shared prompt, generates JSON, and parses it.
func (g *Generator) GenerateItinerary(ctx context.Context, trip types.TripRequest) ([]types.DayPlan, error) {
	prompt, err := ai.BuildItineraryPrompt(trip)
	if err != nil {
		return nil, err
	}
	raw, err := g.GenerateJSON(ctx, prompt)
	if err != nil {
		return nil, err
	}
	return ai.ParseItinerary(raw)
}

// GenerateJSON returns cleaned JSON text from a single-shot prompt.
func (g *Generator) GenerateJSON(ctx context.Context, prompt string) (string, error) {
	resp, err := ai.Retry(ctx, 3, func(ctx context.Context) (*genai.GenerateContentResponse, error) {
		c, cancel := context.WithTimeout(ctx, g.timeout)
		defer cancel()
		return g.jsonModel().GenerateContent(c, genai.Text(prompt))
	})
	if err != nil {
		return "", fmt.Errorf("gemini: %w", err)
	}
	return ai.CleanJSON(extractText(resp)), nil
}

// Chat runs a multi-turn conversation and returns the model's raw reply.
func (g *Generator) Chat(ctx context.Context, system string, history []ai.ChatTurn, message string) (string, error) {
	m := g.client.GenerativeModel(g.modelName)
	m.SetTemperature(0.8)
	if system != "" {
		m.SystemInstruction = genai.NewUserContent(genai.Text(system))
	}
	chat := m.StartChat()
	for _, h := range history {
		role := h.Role
		if role == "assistant" {
			role = "model"
		}
		chat.History = append(chat.History, &genai.Content{Role: role, Parts: []genai.Part{genai.Text(h.Content)}})
	}
	resp, err := ai.Retry(ctx, 2, func(ctx context.Context) (*genai.GenerateContentResponse, error) {
		c, cancel := context.WithTimeout(ctx, g.timeout)
		defer cancel()
		return chat.SendMessage(c, genai.Text(message))
	})
	if err != nil {
		return "", fmt.Errorf("gemini: %w", err)
	}
	return extractText(resp), nil
}

// Close releases the underlying client.
func (g *Generator) Close() error {
	if g.client != nil {
		return g.client.Close()
	}
	return nil
}

func extractText(resp *genai.GenerateContentResponse) string {
	if resp == nil || len(resp.Candidates) == 0 || resp.Candidates[0].Content == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range resp.Candidates[0].Content.Parts {
		if t, ok := p.(genai.Text); ok {
			b.WriteString(string(t))
		}
	}
	return b.String()
}

// toolResultParts builds the Gemini FunctionResponse parts for a "tool" role
// turn. Gemini matches a response to its originating call by function name (it
// has no tool-call IDs), so we prefer ChatTurn.Name and fall back to
// ToolCallID for callers that stash the name there.
func toolResultParts(h ai.ChatTurn) []genai.Part {
	name := h.Name
	if name == "" {
		name = h.ToolCallID
	}
	if name != "" {
		var res map[string]interface{}
		if err := json.Unmarshal([]byte(h.Content), &res); err != nil {
			res = map[string]interface{}{"result": h.Content}
		}
		return []genai.Part{genai.FunctionResponse{Name: name, Response: res}}
	}
	var parts []genai.Part
	for _, tc := range h.ToolCalls {
		parts = append(parts, genai.FunctionResponse{Name: tc.Name, Response: tc.Arguments})
	}
	return parts
}

func (g *Generator) ChatWithTools(ctx context.Context, system string, history []ai.ChatTurn, tools []ai.ToolDefinition) (ai.ChatResponse, error) {
	m := g.client.GenerativeModel(g.modelName)
	m.SetTemperature(0.5)
	if system != "" {
		m.SystemInstruction = genai.NewUserContent(genai.Text(system))
	}
	m.Tools = convertTools(tools)

	chat := m.StartChat()
	if len(history) == 0 {
		return ai.ChatResponse{}, fmt.Errorf("gemini: history must not be empty for ChatWithTools")
	}

	last := history[len(history)-1]
	history = history[:len(history)-1]

	for _, h := range history {
		role := h.Role
		if role == "assistant" {
			role = "model"
		}

		if role == "tool" {
			chat.History = append(chat.History, &genai.Content{Role: "user", Parts: toolResultParts(h)})
		} else if len(h.ToolCalls) > 0 {
			var parts []genai.Part
			if h.Content != "" {
				parts = append(parts, genai.Text(h.Content))
			}
			for _, tc := range h.ToolCalls {
				parts = append(parts, genai.FunctionCall{
					Name: tc.Name,
					Args: tc.Arguments,
				})
			}
			chat.History = append(chat.History, &genai.Content{Role: "model", Parts: parts})
		} else {
			chat.History = append(chat.History, &genai.Content{Role: role, Parts: []genai.Part{genai.Text(h.Content)}})
		}
	}

	var parts []genai.Part
	if last.Role == "tool" {
		parts = toolResultParts(last)
	} else if len(last.ToolCalls) > 0 {
		if last.Content != "" {
			parts = append(parts, genai.Text(last.Content))
		}
		for _, tc := range last.ToolCalls {
			parts = append(parts, genai.FunctionCall{
				Name: tc.Name,
				Args: tc.Arguments,
			})
		}
	} else {
		parts = append(parts, genai.Text(last.Content))
	}

	resp, err := ai.Retry(ctx, 2, func(ctx context.Context) (*genai.GenerateContentResponse, error) {
		c, cancel := context.WithTimeout(ctx, g.timeout)
		defer cancel()
		return chat.SendMessage(c, parts...)
	})
	if err != nil {
		return ai.ChatResponse{}, fmt.Errorf("gemini: %w", err)
	}

	if resp == nil || len(resp.Candidates) == 0 || resp.Candidates[0].Content == nil {
		return ai.ChatResponse{}, nil
	}

	var out ai.ChatResponse
	for _, p := range resp.Candidates[0].Content.Parts {
		switch v := p.(type) {
		case genai.Text:
			out.Content += string(v)
		case genai.FunctionCall:
			out.ToolCalls = append(out.ToolCalls, ai.ToolCall{
				Name:      v.Name,
				Arguments: v.Args,
			})
		}
	}

	return out, nil
}

var _ ai.LLMProvider = (*Generator)(nil)
