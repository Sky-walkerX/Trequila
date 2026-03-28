// Package gemini provides a Gemini-based implementation of the ItineraryGenerator
package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/generative-ai-go/genai"
	"github.com/sky-walkerx/trequila/internal/ai"
	"github.com/sky-walkerx/trequila/internal/types"
	"google.golang.org/api/option"
)

// Generator implements ai.ItineraryGenerator using Google Gemini
type Generator struct {
	client  *genai.Client
	model   *genai.GenerativeModel
	config  ai.Config
	timeout time.Duration
}

// New creates a new Gemini-based itinerary generator
func New(ctx context.Context, cfg ai.Config) (*Generator, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("gemini: API key is required")
	}

	client, err := genai.NewClient(ctx, option.WithAPIKey(cfg.APIKey))
	if err != nil {
		return nil, fmt.Errorf("gemini: failed to create client: %w", err)
	}

	modelName := cfg.Model
	if modelName == "" {
		modelName = "gemini-2.5-flash"
	}

	// CHANGED: Removed "models/" prefix check as client.GenerativeModel handles it
	model := client.GenerativeModel(modelName)
	model.SetTemperature(cfg.Temperature)
	model.SetTopP(cfg.TopP)
	maxTokens := cfg.MaxOutputTokens
	if maxTokens < 16384 {
		maxTokens = 16384
	}
	model.SetMaxOutputTokens(maxTokens)
	// Set response MIME type to ensure structured JSON output (like Next.js responseMimeType)
	model.ResponseMIMEType = "application/json"
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout == 0 {
		timeout = 60 * time.Second
	}

	return &Generator{
		client:  client,
		model:   model,
		config:  cfg,
		timeout: timeout,
	}, nil
}

// GenerateItinerary generates a travel itinerary using Gemini AI with retry logic
func (g *Generator) GenerateItinerary(ctx context.Context, trip types.TripRequest) ([]types.DayPlan, error) {
	prompt, err := g.buildPrompt(trip)
	if err != nil {
		return nil, fmt.Errorf("gemini: failed to build prompt: %w", err)
	}

	var resp *genai.GenerateContentResponse
	const maxRetries = 3

	for attempt := 0; attempt <= maxRetries; attempt++ {
		genCtx, cancel := context.WithTimeout(ctx, g.timeout)
		resp, err = g.model.GenerateContent(genCtx, genai.Text(prompt))
		cancel()

		if err == nil {
			break
		}

		// Retry on rate limit (429) or server errors (5xx)
		errMsg := err.Error()
		isRetryable := strings.Contains(errMsg, "429") ||
			strings.Contains(errMsg, "503") ||
			strings.Contains(errMsg, "500") ||
			strings.Contains(errMsg, "resource exhausted") ||
			strings.Contains(errMsg, "unavailable")

		if !isRetryable || attempt == maxRetries {
			return nil, fmt.Errorf("gemini: API error after %d attempts: %w", attempt+1, err)
		}

		backoff := time.Duration(1<<uint(attempt)) * time.Second // 1s, 2s, 4s
		log.Printf("gemini: retrying in %v (attempt %d/%d): %v", backoff, attempt+1, maxRetries, err)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	return g.parseResponse(resp)
}

// Close releases resources used by the generator
func (g *Generator) Close() error {
	if g.client != nil {
		return g.client.Close()
	}
	return nil
}

// buildPrompt constructs the AI prompt from trip details
func (g *Generator) buildPrompt(trip types.TripRequest) (string, error) {
	startDate, err := time.Parse("2006-01-02", trip.StartDate)
	if err != nil {
		return "", fmt.Errorf("invalid start date: %w", err)
	}

	endDate, err := time.Parse("2006-01-02", trip.EndDate)
	if err != nil {
		return "", fmt.Errorf("invalid end date: %w", err)
	}

	durationDays := int(endDate.Sub(startDate).Hours()/24) + 1
	if trip.Days > 0 {
		durationDays = trip.Days
	}

	startDateFormatted := startDate.Format("January 2, 2006")
	endDateFormatted := endDate.Format("January 2, 2006")

	interests := "General sightseeing"
	if len(trip.Interests) > 0 {
		interests = strings.Join(trip.Interests, ", ")
	}

	budgetStr := "Not specified; use typical travel costs"
	if trip.Budget > 0 {
		budgetStr = fmt.Sprintf("$%d USD (approximate cap)", trip.Budget)
	}

	prompt := fmt.Sprintf(`You are an expert AI travel planner with deep knowledge of destinations worldwide and access to realistic cost data.
Generate a detailed %d-day itinerary for %s, ensuring each day's activities include accurate budget estimates in USD.
Use typical travel costs, local average prices, and standard fees to provide realistic activity costs.

Trip Details:
- Destination: %s
- Duration: %d days (%s to %s)
- Interests: %s
- Total Budget: %s

Return ONLY a valid JSON array with this exact structure (no markdown, no explanation):

[
  {
    "dayIndex": 0,
    "date": "2024-06-15",
    "summary": "Brief day summary",
    "activities": [
      {
        "name": "Activity name",
        "type": "Attraction",
        "time": "9:00 AM",
        "description": "Activity description (under 250 chars)",
        "address": "Full address if known",
        "notes": "Helpful tips, under 200 chars",
        "budget": 25
      }
    ]
  }
]

Requirements:
- Include 3-5 activities per day with realistic timing
- Activity types: Attraction, Restaurant, Transport, Entertainment, Shopping, Nature, Culture
- Provide accurate addresses for well-known locations
- Budget should be per-person cost in USD (integer, no decimals)
- Keep 'description' under 250 characters
- Keep 'notes' under 200 characters
- Dates should follow the format YYYY-MM-DD starting from %s
- Focus on must-see destinations and local experiences
- If budget is specified, ensure total daily spending is reasonable
- Return ONLY the JSON array, no additional text or markdown formatting.`,
		durationDays, trip.Destination,
		trip.Destination,
		durationDays, startDateFormatted, endDateFormatted,
		interests,
		budgetStr,
		trip.StartDate,
	)

	return prompt, nil
}

// parseResponse extracts and parses the itinerary from Gemini's response
func (g *Generator) parseResponse(resp *genai.GenerateContentResponse) ([]types.DayPlan, error) {
	if len(resp.Candidates) == 0 || len(resp.Candidates[0].Content.Parts) == 0 {
		return nil, fmt.Errorf("gemini: empty response")
	}

	// Concatenate all parts of the response
	var fullText strings.Builder
	for _, part := range resp.Candidates[0].Content.Parts {
		if text, ok := part.(genai.Text); ok {
			fullText.WriteString(string(text))
		}
	}

	if fullText.Len() == 0 {
		return nil, fmt.Errorf("gemini: no text content in response")
	}

	jsonStr := cleanJSONResponse(fullText.String())

	var itinerary []types.DayPlan
	if err := json.Unmarshal([]byte(jsonStr), &itinerary); err != nil {
		preview := jsonStr
		if len(preview) > 200 {
			preview = preview[:200] + "..."
		}
		return nil, fmt.Errorf("gemini: failed to parse JSON: %w (preview: %s)", err, preview)
	}

	return itinerary, nil
}

// cleanJSONResponse extracts the JSON array from the response string
// It finds the first '[' and last ']' to ignore Markdown code blocks or conversational text.
func cleanJSONResponse(s string) string {
	s = strings.TrimSpace(s)

	start := strings.Index(s, "[")
	end := strings.LastIndex(s, "]")

	if start != -1 && end != -1 && end > start {
		return s[start : end+1]
	}

	return s
}

// Ensure Generator implements the interface
var _ ai.ItineraryGenerator = (*Generator)(nil)
