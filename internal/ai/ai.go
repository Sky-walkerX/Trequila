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
