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

	// 3. Start Background Consumers
	// A. Listens for completed itineraries
	go server.consumeItineraries()

	// B. Listens for found images
	go server.consumeImages()

	// C. Listens for found places (NEW)
	go server.consumePlaces()

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
	var imagesJSON []byte
	var placesJSON []byte
	var generatedAt sql.NullString

	// Retrieve from DB (Added 'images' and 'places' columns)
	err := s.db.QueryRow(`
        SELECT status, itinerary, images, places, updated_at 
        FROM trips WHERE request_id = $1`, id,
	).Scan(&status, &itineraryJSON, &imagesJSON, &placesJSON, &generatedAt)

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
		resp["itinerary"] = json.RawMessage(itineraryJSON)
	} else {
		resp["itinerary"] = nil
	}

	if len(imagesJSON) > 0 {
		resp["images"] = json.RawMessage(imagesJSON)
	} else {
		resp["images"] = nil
	}

	if len(placesJSON) > 0 {
		resp["places"] = json.RawMessage(placesJSON)
	} else {
		resp["places"] = nil
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// consumeItineraries listens for completed AI jobs and updates the database
func (s *Server) consumeItineraries() {
	ctx := context.Background()
	var cons jetstream.Consumer

	for {
		stream, err := s.js.Stream(ctx, "TRIPS")
		if err == nil {
			cons, err = stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
				Durable:       "api-result-writer-v5",
				FilterSubject: "trips.itinerary",
				AckPolicy:     jetstream.AckExplicitPolicy,
				DeliverPolicy: jetstream.DeliverAllPolicy,
				AckWait:       30 * time.Second,
				MaxDeliver:    5,
			})
			if err == nil {
				break
			}
		}
		log.Printf("Waiting for TRIPS stream (itinerary)... (%v)", err)
		time.Sleep(3 * time.Second)
	}

	log.Println("Background Consumer: Listening for Generated Itineraries...")

	for {
		msgs, err := cons.Fetch(1, jetstream.FetchMaxWait(5*time.Second))
		if err != nil {
			continue
		}

		for msg := range msgs.Messages() {
			var event types.ItineraryGeneratedEvent
			if err := json.Unmarshal(msg.Data(), &event); err != nil {
				log.Printf("Invalid event format: %v", err)
				msg.Ack()
				continue
			}

			itineraryBytes, _ := json.Marshal(event.Data.Itinerary)

			// Cast to string for correct Postgres JSON/JSONB handling
			_, err = s.db.Exec(`
                UPDATE trips 
                SET status = 'completed', itinerary = $1, updated_at = NOW()
                WHERE request_id = $2`,
				string(itineraryBytes), event.Data.RequestID,
			)

			if err != nil {
				log.Printf("DB Update failed for %s: %v", event.Data.RequestID, err)
				msg.Nak()
			} else {
				log.Printf("Itinerary saved for %s", event.Data.RequestID)
				msg.Ack()
			}
		}
	}
}

// consumeImages listens for found images and updates the database
func (s *Server) consumeImages() {
	ctx := context.Background()
	var cons jetstream.Consumer

	// Wait loop for Stream existence
	for {
		stream, err := s.js.Stream(ctx, "TRIPS")
		if err == nil {
			cons, err = stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
				Durable:       "api-image-writer-v1",
				FilterSubject: "trips.images",
				AckPolicy:     jetstream.AckExplicitPolicy,
				AckWait:       30 * time.Second,
			})
			if err == nil {
				break
			}
		}
		log.Printf("Waiting for TRIPS stream (images)... (%v)", err)
		time.Sleep(3 * time.Second)
	}

	log.Println("Background Consumer: Listening for Images...")

	// Helper struct for decoding payload
	type ImagePayload struct {
		Data struct {
			RequestID string          `json:"request_id"`
			Images    json.RawMessage `json:"images"` // Keep as raw JSON to save directly
		} `json:"data"`
	}

	for {
		msgs, err := cons.Fetch(1, jetstream.FetchMaxWait(5*time.Second))
		if err != nil {
			continue
		}

		for msg := range msgs.Messages() {
			var event ImagePayload
			if err := json.Unmarshal(msg.Data(), &event); err != nil {
				log.Printf("Invalid image event format: %v", err)
				msg.Ack()
				continue
			}

			// Update DB with images
			_, err = s.db.Exec(`
                UPDATE trips 
                SET images = $1 
                WHERE request_id = $2`,
				string(event.Data.Images), event.Data.RequestID,
			)

			if err != nil {
				log.Printf("DB Error saving images: %v", err)
				msg.Nak()
			} else {
				log.Printf("Images saved for %s", event.Data.RequestID)
				msg.Ack()
			}
		}
	}
}

// consumePlaces listens for found places and updates the database
func (s *Server) consumePlaces() {
	ctx := context.Background()
	var cons jetstream.Consumer

	// Wait loop for Stream existence
	for {
		stream, err := s.js.Stream(ctx, "TRIPS")
		if err == nil {
			cons, err = stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
				Durable:       "api-places-writer-v1",
				FilterSubject: "trips.places",
				AckPolicy:     jetstream.AckExplicitPolicy,
				AckWait:       30 * time.Second,
			})
			if err == nil {
				break
			}
		}
		log.Printf("Waiting for TRIPS stream (places)... (%v)", err)
		time.Sleep(3 * time.Second)
	}

	log.Println("Background Consumer: Listening for Places...")

	// Helper struct for decoding payload
	type PlacesPayload struct {
		Data struct {
			RequestID string          `json:"request_id"`
			Places    json.RawMessage `json:"places"` // Keep as raw JSON to save directly
		} `json:"data"`
	}

	for {
		msgs, err := cons.Fetch(1, jetstream.FetchMaxWait(5*time.Second))
		if err != nil {
			continue
		}

		for msg := range msgs.Messages() {
			var event PlacesPayload
			if err := json.Unmarshal(msg.Data(), &event); err != nil {
				log.Printf("Invalid places event format: %v", err)
				msg.Ack()
				continue
			}

			// Update DB with places
			_, err = s.db.Exec(`
                UPDATE trips 
                SET places = $1 
                WHERE request_id = $2`,
				string(event.Data.Places), event.Data.RequestID,
			)

			if err != nil {
				log.Printf("DB Error saving places: %v", err)
				msg.Nak()
			} else {
				log.Printf("Places saved for %s", event.Data.RequestID)
				msg.Ack()
			}
		}
	}
}
