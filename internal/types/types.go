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
	CreatedAt   string   `json:"created_at"`
}

// TripEvent represents a NATS event envelope for trip-related messages
type TripEvent struct {
	Type string      `json:"type"`
	Data TripRequest `json:"data"`
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

// Itinerary represents a complete trip itinerary
type Itinerary struct {
	RequestID   string    `json:"request_id"`
	UserID      string    `json:"user_id"`
	Source      string    `json:"source"`
	Destination string    `json:"destination"`
	StartDate   string    `json:"start_date"`
	EndDate     string    `json:"end_date"`
	Days        []DayPlan `json:"days"`
	TotalBudget int       `json:"total_budget"`
	GeneratedAt string    `json:"generated_at"`
}

// FlightSearchParams contains parameters for flight search
type FlightSearchParams struct {
	Origin        string `json:"origin"`
	Destination   string `json:"destination"`
	DepartureDate string `json:"departure_date"`
	ReturnDate    string `json:"return_date"`
}

// HotelSearchParams contains parameters for hotel search
type HotelSearchParams struct {
	Location string `json:"location"`
	CheckIn  string `json:"check_in"`
	CheckOut string `json:"check_out"`
}

// ItineraryGeneratedEvent represents the event published when an itinerary is generated
type ItineraryGeneratedEvent struct {
	Type string                      `json:"type"`
	Data ItineraryGeneratedEventData `json:"data"`
}

// ItineraryGeneratedEventData contains the data for an itinerary generated event
type ItineraryGeneratedEventData struct {
	RequestID          string             `json:"request_id"`
	UserID             string             `json:"user_id"`
	Source             string             `json:"source"`
	Destination        string             `json:"destination"`
	StartDate          string             `json:"start_date"`
	EndDate            string             `json:"end_date"`
	Itinerary          []DayPlan          `json:"itinerary"`
	FlightSearchParams FlightSearchParams `json:"flight_search_params"`
	HotelSearchParams  HotelSearchParams  `json:"hotel_search_params"`
	GeneratedAt        string             `json:"generated_at"`
}
