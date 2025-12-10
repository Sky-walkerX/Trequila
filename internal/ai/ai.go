// Package ai provides interfaces and implementations for AI-powered features
package ai

import (
	"context"

	"github.com/sky-walkerx/trequila/internal/types"
)

// ItineraryGenerator defines the interface for generating travel itineraries
type ItineraryGenerator interface {
	// GenerateItinerary generates a travel itinerary based on the trip request
	GenerateItinerary(ctx context.Context, trip types.TripRequest) ([]types.DayPlan, error)

	// Close cleans up any resources used by the generator
	Close() error
}

// Config holds configuration for AI providers
type Config struct {
	// APIKey is the authentication key for the AI provider
	APIKey string

	// Model is the specific model to use (e.g., "gemini-1.5-flash")
	Model string

	// MaxOutputTokens limits the response size
	MaxOutputTokens int32

	// Temperature controls randomness (0.0 = deterministic, 1.0 = creative)
	Temperature float32

	// TopP controls nucleus sampling
	TopP float32

	// TimeoutSeconds for API calls
	TimeoutSeconds int
}

// DefaultConfig returns a sensible default configuration
func DefaultConfig(apiKey string) Config {
	return Config{
		APIKey:          apiKey,
		Model:           "gemini-2.5-flash",
		MaxOutputTokens: 16384,
		Temperature:     0.7,
		TopP:            0.95,
		TimeoutSeconds:  60,
	}
}
