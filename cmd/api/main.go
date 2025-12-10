package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	natshelper "github.com/sky-walkerx/trequila/internal/nats"
)

type TripRequest struct {
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
	// Initialize JetStream connection
	js := natshelper.ConnectJetStream(nats.DefaultURL)

	// Define HTTP handler for trip requests
	http.HandleFunc("/trips", func(w http.ResponseWriter, r *http.Request) {
		// Only accept POST requests
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Parse incoming JSON request
		var req TripRequest
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

		// Validate date format and calculate days if not provided
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

		// Validate end date is after start date
		if endDate.Before(startDate) || endDate.Equal(startDate) {
			http.Error(w, "end_date must be after start_date", http.StatusBadRequest)
			return
		}

		// Calculate days if not provided
		if req.Days == 0 {
			req.Days = int(endDate.Sub(startDate).Hours() / 24)
		}

		// Generate unique ID for this request
		requestID := uuid.NewString()

		// Create event payload following event-driven pattern
		ev := map[string]interface{}{
			"type": "TripRequested", // Event type
			"data": map[string]interface{}{
				"request_id":  requestID,
				"user_id":     req.UserID,
				"source":      req.Source,
				"destination": req.Destination,
				"start_date":  req.StartDate,
				"end_date":    req.EndDate,
				"days":        req.Days,
				"budget":      req.Budget,
				"interests":   req.Interests,
				"created_at":  time.Now().UTC().Format(time.RFC3339),
			},
		}

		b, _ := json.Marshal(ev)

		// Publish to JetStream with timeout
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		// Publish message to "trips.requested" subject
		// WithMsgID ensures message deduplication within the configured window
		ack, err := js.Publish(ctx, "trips.requested", b,
			jetstream.WithMsgID(requestID), // Prevents duplicate processing
		)
		if err != nil {
			http.Error(w, "publish failed", http.StatusInternalServerError)
			log.Printf("JS publish error: %v", err)
			return
		}

		log.Printf("Published TripRequested event: %s (seq: %d)", requestID, ack.Sequence)

		// Return success response with request ID
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
