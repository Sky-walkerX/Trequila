package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib" // Standard library adapter for pgx
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sky-walkerx/trequila/internal/auth"
	"github.com/sky-walkerx/trequila/internal/metrics"
	"github.com/sky-walkerx/trequila/internal/middleware"
	natshelper "github.com/sky-walkerx/trequila/internal/nats"
	"github.com/sky-walkerx/trequila/internal/types"
	"golang.org/x/time/rate"
)

// Server holds shared dependencies for the API
type Server struct {
	js   jetstream.JetStream
	nc   *nats.Conn
	db   *sql.DB
	auth *auth.AuthService
}

func main() {
	// 1. Initialize NATS
	natsURL := os.Getenv("NATS_URL")
	if natsURL == "" {
		natsURL = nats.DefaultURL
	}
	js, nc := natshelper.ConnectJetStream(natsURL)

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
		js:   js,
		nc:   nc,
		db:   db,
		auth: auth.NewAuthService(db),
	}

	// 3. Start Background Consumers
	// A. Listens for completed itineraries
	go server.consumeItineraries()

	// B. Listens for found images
	go server.consumeImages()

	// C. Listens for found places
	go server.consumePlaces()

	// 4. Setup Middleware
	// Rate limiter: 10 requests per second, burst of 20
	rateLimiter := middleware.NewRateLimiter(rate.Limit(10), 20)

	// CORS config
	corsConfig := middleware.DefaultCORSConfig()
	// In production, set specific origins:
	// corsConfig.AllowedOrigins = []string{"https://your-frontend.com"}

	// 5. Setup Router with middleware
	mux := http.NewServeMux()

	// Public routes (no auth required)
	mux.HandleFunc("/health", server.handleHealth)
	mux.Handle("/metrics", promhttp.Handler())

	// Auth routes
	mux.HandleFunc("/auth/register", server.handleRegister)
	mux.HandleFunc("/auth/login", server.handleLogin)
	mux.HandleFunc("/auth/google/login", server.handleGoogleLogin)
	mux.HandleFunc("/auth/google/callback", server.handleGoogleCallback)
	mux.HandleFunc("/auth/token", server.handleGenerateToken) // For testing/dev
	
	// User profile (protected)
	mux.Handle("/api/me", server.auth.AuthMiddleware(
		http.HandlerFunc(server.handleGetCurrentUser),
	))

	// Protected routes (auth required) - wrap with auth middleware
	mux.Handle("/api/trips", server.auth.AuthMiddleware(
		http.HandlerFunc(server.instrumentHandler("/api/trips", server.handleTrips)),
	))
	mux.Handle("/api/trips/", server.auth.AuthMiddleware(
		http.HandlerFunc(server.instrumentHandler("/api/trips/{id}", server.handleTripByID)),
	))

	// Apply middleware chain: CORS -> RateLimit -> Handler
	handler := middleware.CORS(corsConfig)(rateLimiter.Limit(mux))

	// 6. Create server with graceful shutdown
	srv := &http.Server{
		Addr:         ":8080",
		Handler:      handler,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 60 * time.Second, // Longer for SSE
		IdleTimeout:  60 * time.Second,
	}

	// Start server in goroutine
	go func() {
		log.Println("API server listening on :8080")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("Shutting down server...")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}

	log.Println("Server exited gracefully")
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

// ===== Health Check =====

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	health := map[string]interface{}{
		"status":    "healthy",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"services":  map[string]string{},
	}

	services := health["services"].(map[string]string)

	// Check DB
	if err := s.db.Ping(); err != nil {
		services["database"] = "unhealthy"
		health["status"] = "degraded"
	} else {
		services["database"] = "healthy"
	}

	// Check NATS
	if s.nc.IsConnected() {
		services["nats"] = "healthy"
	} else {
		services["nats"] = "unhealthy"
		health["status"] = "degraded"
	}

	w.Header().Set("Content-Type", "application/json")
	if health["status"] == "healthy" {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	json.NewEncoder(w).Encode(health)
}

// ===== Auth Handlers =====

func (s *Server) handleGoogleLogin(w http.ResponseWriter, r *http.Request) {
	// Generate state for CSRF protection
	state := uuid.NewString()

	// In production, store state in session/cookie for validation
	url := s.auth.GoogleLoginURL(state)
	if url == "" {
		http.Error(w, `{"error":"google oauth not configured"}`, http.StatusNotImplemented)
		return
	}

	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

func (s *Server) handleGoogleCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, `{"error":"missing code parameter"}`, http.StatusBadRequest)
		return
	}

	// In production, validate state parameter here

	user, token, err := s.auth.HandleGoogleCallback(r.Context(), code)
	if err != nil {
		log.Printf("Google callback error: %v", err)
		http.Error(w, `{"error":"authentication failed"}`, http.StatusUnauthorized)
		return
	}

	// Return token to client
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"token": token,
		"user": map[string]string{
			"id":    user.ID,
			"email": user.Email,
			"name":  user.Name,
		},
	})
}

// handleGenerateToken is for testing/development - generates a token for a user
// This creates a real user in the database to avoid FK constraint violations
func (s *Server) handleGenerateToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		UserID string `json:"user_id"`
		Email  string `json:"email"`
		Name   string `json:"name"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	// Generate IDs if not provided
	if req.UserID == "" {
		req.UserID = uuid.NewString()
	}
	if req.Email == "" {
		req.Email = fmt.Sprintf("dev-%s@trequila.local", req.UserID[:8])
	}
	if req.Name == "" {
		req.Name = "Dev User"
	}

	// Create or update user in database to satisfy FK constraint
	_, err := s.db.Exec(`
		INSERT INTO users (id, email, name, provider)
		VALUES ($1, $2, $3, 'dev')
		ON CONFLICT (id) DO UPDATE SET name = $3, updated_at = NOW()`,
		req.UserID, req.Email, req.Name,
	)
	if err != nil {
		// Try with a unique email if there's a conflict
		req.Email = fmt.Sprintf("dev-%s@trequila.local", req.UserID)
		_, err = s.db.Exec(`
			INSERT INTO users (id, email, name, provider)
			VALUES ($1, $2, $3, 'dev')
			ON CONFLICT (id) DO UPDATE SET name = $3, updated_at = NOW()`,
			req.UserID, req.Email, req.Name,
		)
		if err != nil {
			log.Printf("Failed to create dev user: %v", err)
			http.Error(w, `{"error":"failed to create dev user"}`, http.StatusInternalServerError)
			return
		}
	}

	token, err := s.auth.GenerateToken(req.UserID, req.Email, req.Name, "dev")
	if err != nil {
		http.Error(w, `{"error":"failed to generate token"}`, http.StatusInternalServerError)
		return
	}

	log.Printf("Dev token created for user: %s (%s)", req.UserID, req.Email)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"token":   token,
		"user_id": req.UserID,
		"email":   req.Email,
		"name":    req.Name,
		"warning": "This endpoint is for development only. Do not use in production.",
	})
}

// handleRegister handles user registration with email/password
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		Name     string `json:"name"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	user, token, err := s.auth.RegisterUser(r.Context(), req.Email, req.Password, req.Name)
	if err != nil {
		status := http.StatusInternalServerError
		switch err {
		case auth.ErrUserExists:
			status = http.StatusConflict
		case auth.ErrInvalidEmail, auth.ErrPasswordRequired:
			status = http.StatusBadRequest
		}
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), status)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"token": token,
		"user": map[string]string{
			"id":    user.ID,
			"email": user.Email,
			"name":  user.Name,
		},
	})
}

// handleLogin handles user login with email/password
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	user, token, err := s.auth.LoginUser(r.Context(), req.Email, req.Password)
	if err != nil {
		status := http.StatusUnauthorized
		switch err {
		case auth.ErrUserNotFound, auth.ErrInvalidPassword:
			status = http.StatusUnauthorized
		case auth.ErrInvalidEmail, auth.ErrPasswordRequired:
			status = http.StatusBadRequest
		}
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), status)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"token": token,
		"user": map[string]string{
			"id":    user.ID,
			"email": user.Email,
			"name":  user.Name,
		},
	})
}

// handleGetCurrentUser returns the current authenticated user's info
func (s *Server) handleGetCurrentUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	userID := auth.GetUserIDFromContext(r.Context())
	if userID == "" {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	user, err := s.auth.GetUserByID(r.Context(), userID)
	if err != nil {
		if err == auth.ErrUserNotFound {
			http.Error(w, `{"error":"user not found"}`, http.StatusNotFound)
			return
		}
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id":         user.ID,
		"email":      user.Email,
		"name":       user.Name,
		"provider":   user.Provider,
		"avatar_url": user.AvatarURL.String,
		"created_at": user.CreatedAt.Format(time.RFC3339),
	})
}

// ===== Protected Trip Handlers =====

// handleTrips handles GET (list) and POST (create) for authenticated users
func (s *Server) handleTrips(w http.ResponseWriter, r *http.Request) {
	userID := auth.GetUserIDFromContext(r.Context())
	if userID == "" {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.handleListUserTrips(w, r, userID)
	case http.MethodPost:
		s.handleCreateUserTrip(w, r, userID)
	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

// handleTripByID handles GET, DELETE for a specific trip (authenticated)
func (s *Server) handleTripByID(w http.ResponseWriter, r *http.Request) {
	userID := auth.GetUserIDFromContext(r.Context())
	if userID == "" {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	// Extract ID from path: /api/trips/{id}
	path := r.URL.Path
	id := ""

	// Handle both /api/trips/{id} and /api/trips/stream/{id}
	if strings.HasPrefix(path, "/api/trips/stream/") {
		id = strings.TrimPrefix(path, "/api/trips/stream/")
		s.handleStreamTripAuth(w, r, userID, id)
		return
	}

	id = strings.TrimPrefix(path, "/api/trips/")
	if id == "" {
		http.Error(w, `{"error":"trip id required"}`, http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.handleGetUserTrip(w, r, userID, id)
	case http.MethodDelete:
		s.handleDeleteUserTrip(w, r, userID, id)
	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

// handleListUserTrips returns all trips for the authenticated user
func (s *Server) handleListUserTrips(w http.ResponseWriter, r *http.Request, userID string) {
	// Parse query params for pagination
	page := 1
	limit := 10

	if p := r.URL.Query().Get("page"); p != "" {
		if parsed, err := strconv.Atoi(p); err == nil && parsed > 0 {
			page = parsed
		}
	}
	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 && parsed <= 50 {
			limit = parsed
		}
	}
	offset := (page - 1) * limit

	// Get total count
	var total int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM trips WHERE user_id = $1`, userID).Scan(&total)
	if err != nil {
		log.Printf("DB count error: %v", err)
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}

	// Get trips with pagination
	rows, err := s.db.Query(`
		SELECT request_id, source, destination, start_date, end_date, budget, status, created_at
		FROM trips 
		WHERE user_id = $1 
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3`,
		userID, limit, offset,
	)
	if err != nil {
		log.Printf("DB query error: %v", err)
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type TripSummary struct {
		RequestID   string `json:"request_id"`
		Source      string `json:"source"`
		Destination string `json:"destination"`
		StartDate   string `json:"start_date"`
		EndDate     string `json:"end_date"`
		Budget      int    `json:"budget"`
		Status      string `json:"status"`
		CreatedAt   string `json:"created_at"`
	}

	var trips []TripSummary
	for rows.Next() {
		var t TripSummary
		var createdAt time.Time
		if err := rows.Scan(&t.RequestID, &t.Source, &t.Destination, &t.StartDate, &t.EndDate, &t.Budget, &t.Status, &createdAt); err != nil {
			continue
		}
		t.CreatedAt = createdAt.Format(time.RFC3339)
		trips = append(trips, t)
	}

	if trips == nil {
		trips = []TripSummary{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"trips": trips,
		"pagination": map[string]interface{}{
			"page":        page,
			"limit":       limit,
			"total":       total,
			"total_pages": (total + limit - 1) / limit,
		},
	})
}

// handleCreateUserTrip creates a new trip for the authenticated user
func (s *Server) handleCreateUserTrip(w http.ResponseWriter, r *http.Request, userID string) {
	var req struct {
		Source      string   `json:"source"`
		Destination string   `json:"destination"`
		StartDate   string   `json:"start_date"`
		EndDate     string   `json:"end_date"`
		Days        int      `json:"days"`
		Budget      int      `json:"budget"`
		Interests   []string `json:"interests"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	// Validation
	if req.Source == "" || req.Destination == "" || req.StartDate == "" || req.EndDate == "" {
		http.Error(w, `{"error":"missing required fields: source, destination, start_date, end_date"}`, http.StatusBadRequest)
		return
	}

	startDate, err := time.Parse("2006-01-02", req.StartDate)
	if err != nil {
		http.Error(w, `{"error":"invalid start_date format, use YYYY-MM-DD"}`, http.StatusBadRequest)
		return
	}
	endDate, err := time.Parse("2006-01-02", req.EndDate)
	if err != nil {
		http.Error(w, `{"error":"invalid end_date format, use YYYY-MM-DD"}`, http.StatusBadRequest)
		return
	}
	if endDate.Before(startDate) {
		http.Error(w, `{"error":"end_date must be after start_date"}`, http.StatusBadRequest)
		return
	}

	// Calculate days
	if req.Days == 0 {
		req.Days = int(endDate.Sub(startDate).Hours()/24) + 1
	}

	requestID := uuid.NewString()

	// Insert into DB
	_, err = s.db.Exec(`
		INSERT INTO trips (request_id, user_id, source, destination, start_date, end_date, budget, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'pending')`,
		requestID, userID, req.Source, req.Destination, req.StartDate, req.EndDate, req.Budget,
	)
	if err != nil {
		log.Printf("DB Insert error: %v", err)
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}

	// Publish to NATS
	event := types.TripEvent{
		Type: "TripRequested",
		Data: types.TripRequest{
			RequestID:   requestID,
			UserID:      userID,
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
		http.Error(w, `{"error":"failed to queue trip"}`, http.StatusInternalServerError)
		return
	}

	log.Printf("Trip requested by user %s: %s (seq: %d)", userID, requestID, ack.Sequence)
	metrics.TripsCreatedTotal.Inc()
	metrics.NatsMessagesPublished.WithLabelValues("trips.requested").Inc()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"request_id": requestID,
		"status":     "pending",
		"message":    "Trip request accepted",
	})
}

// handleGetUserTrip returns detailed trip info for the authenticated user
func (s *Server) handleGetUserTrip(w http.ResponseWriter, r *http.Request, userID, tripID string) {
	var status string
	var source, destination, startDate, endDate string
	var budget int
	var itineraryJSON, imagesJSON, placesJSON []byte
	var createdAt, updatedAt time.Time

	err := s.db.QueryRow(`
		SELECT status, source, destination, start_date, end_date, budget, 
		       itinerary, images, places, created_at, updated_at
		FROM trips 
		WHERE request_id = $1 AND user_id = $2`,
		tripID, userID,
	).Scan(&status, &source, &destination, &startDate, &endDate, &budget,
		&itineraryJSON, &imagesJSON, &placesJSON, &createdAt, &updatedAt)

	if err == sql.ErrNoRows {
		http.Error(w, `{"error":"trip not found"}`, http.StatusNotFound)
		return
	} else if err != nil {
		log.Printf("DB Query error: %v", err)
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}

	resp := map[string]interface{}{
		"request_id":  tripID,
		"status":      status,
		"source":      source,
		"destination": destination,
		"start_date":  startDate,
		"end_date":    endDate,
		"budget":      budget,
		"created_at":  createdAt.Format(time.RFC3339),
		"updated_at":  updatedAt.Format(time.RFC3339),
	}

	if len(itineraryJSON) > 0 {
		resp["itinerary"] = json.RawMessage(itineraryJSON)
	}
	if len(imagesJSON) > 0 {
		resp["images"] = json.RawMessage(imagesJSON)
	}
	if len(placesJSON) > 0 {
		resp["places"] = json.RawMessage(placesJSON)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleDeleteUserTrip deletes a trip for the authenticated user
func (s *Server) handleDeleteUserTrip(w http.ResponseWriter, r *http.Request, userID, tripID string) {
	result, err := s.db.Exec(`DELETE FROM trips WHERE request_id = $1 AND user_id = $2`, tripID, userID)
	if err != nil {
		log.Printf("DB Delete error: %v", err)
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		http.Error(w, `{"error":"trip not found"}`, http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message": "trip deleted successfully",
	})
}

// handleStreamTripAuth streams SSE for authenticated users
func (s *Server) handleStreamTripAuth(w http.ResponseWriter, r *http.Request, userID, tripID string) {
	// Verify user owns this trip
	var exists bool
	err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM trips WHERE request_id = $1 AND user_id = $2)`, tripID, userID).Scan(&exists)
	if err != nil || !exists {
		http.Error(w, `{"error":"trip not found"}`, http.StatusNotFound)
		return
	}

	// Delegate to the existing SSE handler logic
	s.handleStreamTripInternal(w, r, tripID)
}

// instrumentHandler wraps handlers with Prometheus metrics
func (s *Server) instrumentHandler(path string, handler func(http.ResponseWriter, *http.Request)) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Wrap ResponseWriter to capture status code
		wrapped := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		handler(wrapped, r)

		duration := time.Since(start).Seconds()
		status := strconv.Itoa(wrapped.statusCode)

		metrics.HttpRequestsTotal.WithLabelValues(r.Method, path, status).Inc()
		metrics.HttpRequestDuration.WithLabelValues(r.Method, path).Observe(duration)
	}
}

// responseWriter wraps http.ResponseWriter to capture status code
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// handleStreamTripInternal is the shared SSE logic
func (s *Server) handleStreamTripInternal(w http.ResponseWriter, r *http.Request, id string) {
	// Fetch current state from database
	var status string
	var imagesJSON, placesJSON, itineraryJSON []byte
	err := s.db.QueryRow(`
		SELECT status, images, places, itinerary 
		FROM trips WHERE request_id = $1`, id,
	).Scan(&status, &imagesJSON, &placesJSON, &itineraryJSON)

	if err == sql.ErrNoRows {
		http.Error(w, "trip not found", http.StatusNotFound)
		return
	} else if err != nil {
		log.Printf("DB Query error: %v", err)
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	// Send initial connection event
	fmt.Fprintf(w, "event: connected\ndata: {\"request_id\":\"%s\",\"message\":\"Stream connected\"}\n\n", id)
	flusher.Flush()

	// Track SSE connection
	metrics.SSEConnectionsActive.Inc()
	defer metrics.SSEConnectionsActive.Dec()

	// Track what we've received - check DB for already available data
	receivedImages := len(imagesJSON) > 0
	receivedPlaces := len(placesJSON) > 0
	receivedItinerary := len(itineraryJSON) > 0

	// Send already-available data from database immediately
	if receivedImages {
		data, _ := json.Marshal(map[string]interface{}{
			"type":   "images",
			"images": json.RawMessage(imagesJSON),
		})
		fmt.Fprintf(w, "event: images\ndata: %s\n\n", data)
		flusher.Flush()
		metrics.SSEEventsTotal.WithLabelValues("images").Inc()
		log.Printf("SSE sent cached images for %s", id)
	}

	if receivedPlaces {
		data, _ := json.Marshal(map[string]interface{}{
			"type":   "places",
			"places": json.RawMessage(placesJSON),
		})
		fmt.Fprintf(w, "event: places\ndata: %s\n\n", data)
		flusher.Flush()
		metrics.SSEEventsTotal.WithLabelValues("places").Inc()
		log.Printf("SSE sent cached places for %s", id)
	}

	if receivedItinerary {
		data, _ := json.Marshal(map[string]interface{}{
			"type":      "itinerary",
			"itinerary": json.RawMessage(itineraryJSON),
		})
		fmt.Fprintf(w, "event: itinerary\ndata: %s\n\n", data)
		flusher.Flush()
		metrics.SSEEventsTotal.WithLabelValues("itinerary").Inc()
		log.Printf("SSE sent cached itinerary for %s", id)

		// All data already available - complete immediately
		fmt.Fprintf(w, "event: complete\ndata: {\"message\":\"All data received\"}\n\n")
		flusher.Flush()
		log.Printf("SSE stream complete (from cache) for %s", id)
		return
	}

	// Subscribe to all trip events for this request using wildcard
	// We'll filter by request_id in the message payload
	subject := "trips.>" // Wildcard to catch images, places, itinerary

	msgChan := make(chan *nats.Msg, 64)
	sub, err := s.nc.ChanSubscribe(subject, msgChan)
	if err != nil {
		log.Printf("SSE Subscribe error: %v", err)
		http.Error(w, "subscription error", http.StatusInternalServerError)
		return
	}
	defer sub.Unsubscribe()

	// Track SSE connection
	metrics.SSEConnectionsActive.Inc()
	defer metrics.SSEConnectionsActive.Dec()

	// Create a context that cancels when client disconnects
	ctx := r.Context()

	// Timeout after 2 minutes (safety net)
	timeout := time.After(2 * time.Minute)

	log.Printf("SSE stream started for %s", id)

	for {
		select {
		case <-ctx.Done():
			log.Printf("SSE client disconnected for %s", id)
			return

		case <-timeout:
			fmt.Fprintf(w, "event: timeout\ndata: {\"message\":\"Stream timeout\"}\n\n")
			flusher.Flush()
			log.Printf("SSE timeout for %s", id)
			return

		case msg := <-msgChan:
			// Parse the event to check if it's for our request_id
			var envelope struct {
				Type string `json:"type"`
				Data struct {
					RequestID string          `json:"request_id"`
					Images    json.RawMessage `json:"images,omitempty"`
					Places    json.RawMessage `json:"places,omitempty"`
					Itinerary json.RawMessage `json:"itinerary,omitempty"`
				} `json:"data"`
			}

			if err := json.Unmarshal(msg.Data, &envelope); err != nil {
				continue
			}

			// Skip if not for our request
			if envelope.Data.RequestID != id {
				continue
			}

			// Determine event type and send appropriate SSE event
			switch msg.Subject {
			case "trips.images":
				if !receivedImages {
					receivedImages = true
					data, _ := json.Marshal(map[string]interface{}{
						"type":   "images",
						"images": envelope.Data.Images,
					})
					fmt.Fprintf(w, "event: images\ndata: %s\n\n", data)
					flusher.Flush()
					metrics.SSEEventsTotal.WithLabelValues("images").Inc()
					log.Printf("SSE sent images for %s", id)
				}

			case "trips.places":
				if !receivedPlaces {
					receivedPlaces = true
					data, _ := json.Marshal(map[string]interface{}{
						"type":   "places",
						"places": envelope.Data.Places,
					})
					fmt.Fprintf(w, "event: places\ndata: %s\n\n", data)
					flusher.Flush()
					metrics.SSEEventsTotal.WithLabelValues("places").Inc()
					log.Printf("SSE sent places for %s", id)
				}

			case "trips.itinerary":
				if !receivedItinerary {
					receivedItinerary = true
					data, _ := json.Marshal(map[string]interface{}{
						"type":      "itinerary",
						"itinerary": envelope.Data.Itinerary,
					})
					fmt.Fprintf(w, "event: itinerary\ndata: %s\n\n", data)
					flusher.Flush()
					metrics.SSEEventsTotal.WithLabelValues("itinerary").Inc()
					log.Printf("SSE sent itinerary for %s", id)

					// Itinerary is the final piece - send complete and close
					fmt.Fprintf(w, "event: complete\ndata: {\"message\":\"All data received\"}\n\n")
					flusher.Flush()
					log.Printf("SSE stream complete for %s", id)
					return
				}
			}
		}
	}
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
