// Package types contains shared data structures used across the application
package types

// TripRequest represents a user's trip planning request
type TripRequest struct {
	RequestID   string   `json:"request_id"`
	UserID      string   `json:"user_id"`
	Source      string   `json:"source"`
	Destination string   `json:"destination"`
	StartDate   string   `json:"start_date"` // Format: YYYY-MM-DD
	EndDate     string   `json:"end_date"`   // Format: YYYY-MM-DD
	Days        int      `json:"days"`
	Budget      int      `json:"budget"`
	Interests   []string `json:"interests"`
	Provider    string   `json:"provider"`
	Model       string   `json:"model"`
	CreatedAt   string   `json:"created_at"`
}

// Activity represents a single activity in a day's itinerary
type Activity struct {
	Name        string `json:"name"`
	Type        string `json:"type"` // Attraction, Restaurant, Transport, Entertainment, Shopping, Nature, Culture
	Time        string `json:"time"`
	Description string `json:"description"`
	Address     string `json:"address"`
	Notes       string `json:"notes"`
	Budget      int    `json:"budget"` // Cost in USD
}

// DayPlan represents a single day in the itinerary
type DayPlan struct {
	DayIndex   int        `json:"dayIndex"`
	Date       string     `json:"date"` // Format: YYYY-MM-DD
	Summary    string     `json:"summary"`
	Activities []Activity `json:"activities"`
}
