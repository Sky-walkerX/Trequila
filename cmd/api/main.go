// API Gateway - accepts trip requests and publishes them to NATS
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	natshelper "github.com/sky-walkerx/trequila/internal/nats"
	"github.com/sky-walkerx/trequila/internal/types"
)

// APITripRequest represents the incoming HTTP request body
type APITripRequest struct {
	UserID      string   `json:"user_id"`
	Source      string   `json:"source"`
	Destination string   `json:"destination"`
	StartDate   string   `json:"start_date"` // Format: YYYY-MM-DD
	EndDate     string   `json:"end_date"`   // Format: YYYY-MM-DD
	Days        int      `json:"days"`       // Can be calculated from dates
	Budget      int      `json:"budget"`
	Interests   []string `json:"interests"`
}

func main() {
	// Get NATS URL from environment or use default
	natsURL := os.Getenv("NATS_URL")
	if natsURL == "" {
		natsURL = nats.DefaultURL
	}

	// Initialize JetStream connection
	js := natshelper.ConnectJetStream(natsURL)

	// Define HTTP handler for trip requests
	http.HandleFunc("/trips", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req APITripRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		// Validate required fields
		if req.UserID == "" || req.Source == "" || req.Destination == "" ||
			req.StartDate == "" || req.EndDate == "" {
			http.Error(w, "missing required fields", http.StatusBadRequest)
			return
		}

		// Validate date format
		startDate, err := time.Parse("2006-01-02", req.StartDate)
		if err != nil {
			http.Error(w, "invalid start_date format (expected YYYY-MM-DD)", http.StatusBadRequest)
			return
		}

		endDate, err := time.Parse("2006-01-02", req.EndDate)
		if err != nil {
			http.Error(w, "invalid end_date format (expected YYYY-MM-DD)", http.StatusBadRequest)
			return
		}

		if endDate.Before(startDate) || endDate.Equal(startDate) {
			http.Error(w, "end_date must be after start_date", http.StatusBadRequest)
			return
		}

		// Calculate days if not provided
		if req.Days == 0 {
			req.Days = int(endDate.Sub(startDate).Hours() / 24)
		}

		// Generate unique request ID
		requestID := uuid.NewString()

		// Create typed event using shared types
		event := types.TripEvent{
			Type: "TripRequested",
			Data: types.TripRequest{
				RequestID:   requestID,
				UserID:      req.UserID,
				Source:      req.Source,
				Destination: req.Destination,
				StartDate:   req.StartDate,
				EndDate:     req.EndDate,
				Days:        req.Days,
				Budget:      req.Budget,
				Interests:   req.Interests,
				CreatedAt:   time.Now().UTC().Format(time.RFC3339),
			},
		}

		data, _ := json.Marshal(event)

		// Publish to JetStream
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		ack, err := js.Publish(ctx, "trips.requested", data,
			jetstream.WithMsgID(requestID),
		)
		if err != nil {
			http.Error(w, "publish failed", http.StatusInternalServerError)
			log.Printf("JS publish error: %v", err)
			return
		}

		log.Printf("Published TripRequested event: %s (seq: %d)", requestID, ack.Sequence)

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w,
			`{"request_id":"%s","seq":%d,"status":"processing"}`,
			requestID,
			ack.Sequence,
		)
	})

	log.Println("API server listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
