package ai

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sky-walkerx/trequila/internal/types"
)

// CleanJSON extracts the outermost JSON value (object or array) from an LLM
// response, ignoring markdown code fences or prose surrounding it.
func CleanJSON(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)

	obj := strings.Index(s, "{")
	arr := strings.Index(s, "[")
	switch {
	case arr != -1 && (obj == -1 || arr < obj):
		if end := strings.LastIndex(s, "]"); end > arr {
			return s[arr : end+1]
		}
	case obj != -1:
		if end := strings.LastIndex(s, "}"); end > obj {
			return s[obj : end+1]
		}
	}
	return s
}

// ParseItinerary parses a day-plan array out of an LLM JSON response.
func ParseItinerary(raw string) ([]types.DayPlan, error) {
	js := CleanJSON(raw)
	var days []types.DayPlan
	if err := json.Unmarshal([]byte(js), &days); err != nil {
		preview := js
		if len(preview) > 200 {
			preview = preview[:200] + "..."
		}
		return nil, fmt.Errorf("ai: failed to parse itinerary JSON: %w (preview: %s)", err, preview)
	}
	return days, nil
}

// BuildItineraryPrompt builds the provider-agnostic itinerary-generation prompt.
func BuildItineraryPrompt(trip types.TripRequest) (string, error) {
	startDate, err := time.Parse("2006-01-02", trip.StartDate)
	if err != nil {
		return "", fmt.Errorf("ai: invalid start date: %w", err)
	}
	endDate, err := time.Parse("2006-01-02", trip.EndDate)
	if err != nil {
		return "", fmt.Errorf("ai: invalid end date: %w", err)
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
