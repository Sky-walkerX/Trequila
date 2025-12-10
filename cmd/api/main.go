// cmd/api/main.go
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib" // Standard library adapter for pgx
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	natshelper "github.com/sky-walkerx/trequila/internal/nats"
	"github.com/sky-walkerx/trequila/internal/types"
)

// Server holds shared dependencies for the API
type Server struct {
	js jetstream.JetStream
	db *sql.DB
}

// APITripRequest represents the incoming HTTP request body
type APITripRequest struct {
	UserID      string   `json:"user_id"`
	Source      string   `json:"source"`
	Destination string   `json:"destination"`
	StartDate   string   `json:"start_date"`
	EndDate     string   `json:"end_date"`
	Days        int      `json:"days"`
	Budget      int      `json:"budget"`
	Interests   []string `json:"interests"`
}

func main() {
	// 1. Initialize NATS
	natsURL := os.Getenv("NATS_URL")
	if natsURL == "" {
		natsURL = nats.DefaultURL
	}
	js := natshelper.ConnectJetStream(natsURL)

	// 2. Initialize Postgres
	dbURL := os.Getenv("DB_URL")
	if dbURL == "" {
		// Default for local docker-compose
		dbURL = "postgres://postgres:password@db:5432/trequila?sslmode=disable"
	}

	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		log.Fatalf("Unable to parse DB URL: %v", err)
	}
	defer db.Close()

	// Wait for DB to be healthy
	if err := waitForDB(db); err != nil {
		log.Fatalf("Could not connect to database: %v", err)
	}

	server := &Server{
		js: js,
		db: db,
	}

	// 3. Start Background Consumer
	// This listens for completed itineraries and saves them to Postgres
	go server.consumeItineraries()

	// 4. Setup Routes
	http.HandleFunc("/trips", server.handleCreateTrip) // POST: Create request
	http.HandleFunc("/trips/", server.handleGetTrip)   // GET:  Check status/result

	log.Println("API server listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

// waitForDB loops until the database is ready
func waitForDB(db *sql.DB) error {
	var err error
	for i := 0; i < 15; i++ {
		if err = db.Ping(); err == nil {
			log.Println("Connected to PostgreSQL")
			return nil
		}
		log.Printf("Waiting for DB... (%v)", err)
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("timeout waiting for db")
}

// handleCreateTrip validates input, saves 'pending' state to DB, and publishes to NATS
func (s *Server) handleCreateTrip(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req APITripRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// Validation Logic
	if req.UserID == "" || req.Source == "" || req.Destination == "" || req.StartDate == "" || req.EndDate == "" {
		http.Error(w, "missing required fields", http.StatusBadRequest)
		return
	}

	startDate, err := time.Parse("2006-01-02", req.StartDate)
	if err != nil {
		http.Error(w, "invalid start_date format", http.StatusBadRequest)
		return
	}
	endDate, err := time.Parse("2006-01-02", req.EndDate)
	if err != nil {
		http.Error(w, "invalid end_date format", http.StatusBadRequest)
		return
	}
	if endDate.Before(startDate) {
		http.Error(w, "end_date must be after start_date", http.StatusBadRequest)
		return
	}

	// Calculate Duration
	if req.Days == 0 {
		req.Days = int(endDate.Sub(startDate).Hours()/24) + 1
	}

	requestID := uuid.NewString()

	// A. Insert into Database (Status: Pending)
	_, err = s.db.Exec(`
		INSERT INTO trips (request_id, user_id, source, destination, start_date, end_date, budget, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'pending')`,
		requestID, req.UserID, req.Source, req.Destination, req.StartDate, req.EndDate, req.Budget,
	)
	if err != nil {
		log.Printf("DB Insert error: %v", err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}

	// B. Publish to NATS
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
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	ack, err := s.js.Publish(ctx, "trips.requested", data, jetstream.WithMsgID(requestID))
	if err != nil {
		log.Printf("NATS Publish error: %v", err)
		// Note: Record exists in DB but failed to queue. Ideally, you'd delete from DB or have a sweeper.
		http.Error(w, "queue error", http.StatusInternalServerError)
		return
	}

	log.Printf("Trip requested: %s (seq: %d)", requestID, ack.Sequence)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, `{"request_id":"%s", "status":"pending", "message":"Request accepted"}`, requestID)
}

// handleGetTrip retrieves a trip and its itinerary by ID
func (s *Server) handleGetTrip(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract ID from path: /trips/{id}
	id := r.URL.Path[len("/trips/"):]
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}

	var status string
	var itineraryJSON []byte
	var generatedAt sql.NullString

	// Retrieve from DB
	err := s.db.QueryRow(`
		SELECT status, itinerary, updated_at 
		FROM trips WHERE request_id = $1`, id,
	).Scan(&status, &itineraryJSON, &generatedAt)

	if err == sql.ErrNoRows {
		http.Error(w, "trip not found", http.StatusNotFound)
		return
	} else if err != nil {
		log.Printf("DB Query error: %v", err)
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}

	// Format response
	resp := make(map[string]interface{})
	resp["request_id"] = id
	resp["status"] = status

	if len(itineraryJSON) > 0 {
		resp["itinerary"] = json.RawMessage(itineraryJSON) // Embeds raw JSON without double-escaping
	} else {
		resp["itinerary"] = nil
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// consumeItineraries listens for completed AI jobs and updates the database
func (s *Server) consumeItineraries() {
	ctx := context.Background()
	var cons jetstream.Consumer

	// 1. Retry Loop: Wait for the TRIPS stream to be created by the Planner
	// If API starts first, this loop prevents it from crashing.
	for {
		stream, err := s.js.Stream(ctx, "TRIPS")
		if err == nil {
			cons, err = stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
				Durable:       "api-result-writer-v4", // Bumped version to reset consumer state
				FilterSubject: "trips.itinerary",
				AckPolicy:     jetstream.AckExplicitPolicy,
				DeliverPolicy: jetstream.DeliverAllPolicy, // Deliver ALL messages, including old ones
				AckWait:       30 * time.Second,           // How long to wait for ack before redelivery
				MaxDeliver:    5,                          // Max redelivery attempts
			})
			if err == nil {
				break // Successfully connected!
			}
		}
		log.Printf("Waiting for TRIPS stream... (%v)", err)
		time.Sleep(3 * time.Second)
	}

	log.Println("Background Consumer: Listening for Generated Itineraries...")

	// 2. Processing Loop
	for {
		// Fetch 1 message, waiting up to 5 seconds
		msgs, err := cons.Fetch(1, jetstream.FetchMaxWait(5*time.Second))
		if err != nil {
			// Normal timeout (no messages) or connection issue; just retry
			continue
		}

		// SAFER LOOP: Use 'range' to handle the batch.
		// This prevents the nil-pointer panic if the batch is empty.
		for msg := range msgs.Messages() {
			var event types.ItineraryGeneratedEvent
			if err := json.Unmarshal(msg.Data(), &event); err != nil {
				log.Printf("Invalid event format: %v", err)
				msg.Ack() // Ack bad data to remove it
				continue
			}

			// Update Database
			itineraryBytes, _ := json.Marshal(event.Data.Itinerary)

			_, err = s.db.Exec(`
				UPDATE trips 
				SET status = 'completed', itinerary = $1, updated_at = NOW()
				WHERE request_id = $2`,
				itineraryBytes, event.Data.RequestID,
			)

			if err != nil {
				log.Printf("DB Update failed for %s: %v", event.Data.RequestID, err)
				msg.Nak() // Retry later
			} else {
				log.Printf("Itinerary saved for %s", event.Data.RequestID)
				msg.Ack()
			}
		}
	}
}
