// cmd/monolith/main.go
// Monolith service - combines API, Planner, Imager, and Places into a single binary
// Uses external NATS for event-driven communication between goroutines
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
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sky-walkerx/trequila/internal/ai"
	"github.com/sky-walkerx/trequila/internal/ai/gemini"
	"github.com/sky-walkerx/trequila/internal/auth"
	"github.com/sky-walkerx/trequila/internal/geo"
	"github.com/sky-walkerx/trequila/internal/images"
	"github.com/sky-walkerx/trequila/internal/metrics"
	"github.com/sky-walkerx/trequila/internal/middleware"
	natshelper "github.com/sky-walkerx/trequila/internal/nats"
	"github.com/sky-walkerx/trequila/internal/planner"
	"github.com/sky-walkerx/trequila/internal/vibes"
	"github.com/sky-walkerx/trequila/internal/types"
	"golang.org/x/time/rate"
)

// Server holds shared dependencies
type Server struct {
	js   jetstream.JetStream
	nc   *nats.Conn
	db   *sql.DB
	auth *auth.AuthService
}

func main() {
	log.Println("=== Trequila Monolith Starting ===")

	// Create root context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. Load and validate environment variables
	config := loadConfig()

	// 2. Initialize NATS JetStream (external service)
	log.Println("Connecting to NATS...")
	js, nc := natshelper.ConnectJetStream(config.NatsURL, config.NatsCredsFile, config.NatsCreds)
	log.Println("Connected to NATS")

	// 3. Initialize PostgreSQL
	log.Println("Connecting to PostgreSQL...")
	db, err := sql.Open("pgx", config.DatabaseURL)
	if err != nil {
		log.Fatalf("Unable to parse DB URL: %v", err)
	}
	defer db.Close()

	// Configure connection pool for better performance
	db.SetMaxOpenConns(25)                 // Max open connections
	db.SetMaxIdleConns(10)                 // Keep 10 idle connections ready
	db.SetConnMaxLifetime(5 * time.Minute) // Recycle connections every 5 mins

	if err := waitForDB(db); err != nil {
		log.Fatalf("Could not connect to database: %v", err)
	}
	log.Println("Connected to PostgreSQL")

	// 4. Initialize services
	server := &Server{
		js:   js,
		nc:   nc,
		db:   db,
		auth: auth.NewAuthService(db),
	}

	// 5. Start background workers (microservices as goroutines)

	// API consumers (save results to DB)
	go server.consumeItineraries(ctx)
	go server.consumeImages(ctx)
	go server.consumePlaces(ctx)
	go server.consumeVibes(ctx)
	log.Println("Started API consumers")

	// Planner worker (AI itinerary generation)
	go startPlannerWorker(ctx, js, config.GeminiAPIKey)
	log.Println("Started Planner worker")

	// Imager worker (fetch destination images)
	go startImagerWorker(ctx, js, config.PexelsAPIKey, config.RedisAddr)
	log.Println("Started Imager worker")

	// Places worker (fetch POIs)
	go startPlacesWorker(ctx, js, config.GeoapifyAPIKey, config.RedisAddr)
	log.Println("Started Places worker")

	// Vibes worker (Analyze Safety/Weather/Crowds)
	go startVibesWorker(ctx, js, config.GeminiVibesKey)
    log.Println("Started Vibes worker")

	// 6. Setup HTTP server
	handler := server.setupRoutes()

	port := config.Port
	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      handler,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 120 * time.Second, // Longer for SSE
		IdleTimeout:  60 * time.Second,
	}

	// Start HTTP server
	go func() {
		log.Printf("HTTP server listening on :%s", port)
		log.Println("=== Trequila Monolith Ready ===")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// 7. Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down...")
	cancel() // Cancel context to stop all workers

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("Server forced to shutdown: %v", err)
	}

	log.Println("Trequila Monolith stopped")
}

// Config holds all configuration
type Config struct {
	Port           string
	NatsURL        string
	NatsCredsFile  string
	NatsCreds      string // Inline credentials content (for Heroku)
	DatabaseURL    string
	RedisAddr      string
	GeminiAPIKey   string
	PexelsAPIKey   string
	GeoapifyAPIKey string
	JWTSecret      string
	GeminiVibesKey string
}

func loadConfig() Config {
	config := Config{
		Port:           getEnv("PORT", "8080"),
		NatsURL:        getEnv("NATS_URL", nats.DefaultURL),
		NatsCredsFile:  os.Getenv("NATS_CREDS_FILE"), // Path to .creds file (local)
		NatsCreds:      os.Getenv("NATS_CREDS"),      // Inline creds content (Heroku)
		DatabaseURL:    getEnv("DATABASE_URL", getEnv("DB_URL", "postgres://postgres:password@localhost:5432/trequila?sslmode=disable")),
		RedisAddr:      getEnv("REDIS_URL", getEnv("REDIS_ADDR", "localhost:6379")),
		GeminiAPIKey:   os.Getenv("GEMINI_API_KEY"),
		PexelsAPIKey:   os.Getenv("PEXELS_API_KEY"),
		GeoapifyAPIKey: os.Getenv("GEOAPIFY_API_KEY"),
		JWTSecret:      getEnv("JWT_SECRET", "dev-secret-change-in-production"),
		GeminiVibesKey: os.Getenv("GEMINI_VIBES_KEY"),
	}

	// Validate required keys
	if config.GeminiAPIKey == "" {
		log.Println("WARNING: GEMINI_API_KEY not set - Planner will fail")
	}
	if config.PexelsAPIKey == "" {
		log.Println("WARNING: PEXELS_API_KEY not set - Imager will fail")
	}
	if config.GeoapifyAPIKey == "" {
		log.Println("WARNING: GEOAPIFY_API_KEY not set - Places will fail")
	}
	if config.GeminiVibesKey == "" {
		log.Println("WARNING: GEMINI_VIBES_KEY not set - Vibes will fail")
	}

	return config
}

func getEnv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func waitForDB(db *sql.DB) error {
	for i := 0; i < 30; i++ {
		if err := db.Ping(); err == nil {
			return nil
		}
		log.Printf("Waiting for database... (attempt %d/30)", i+1)
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("timeout waiting for database")
}

// =====================================================
// PLANNER WORKER
// =====================================================

func startPlannerWorker(ctx context.Context, js jetstream.JetStream, apiKey string) {
	if apiKey == "" {
		log.Println("Planner: Skipping - no API key")
		return
	}

	// Initialize Gemini AI
	aiConfig := ai.DefaultConfig(apiKey)
	generator, err := gemini.New(ctx, aiConfig)
	if err != nil {
		log.Printf("Planner: Failed to initialize Gemini: %v", err)
		return
	}
	defer generator.Close()

	// Create planner service
	plannerConfig := planner.DefaultConfig()
	svc, err := planner.New(js, generator, plannerConfig)
	if err != nil {
		log.Printf("Planner: Failed to create service: %v", err)
		return
	}
	defer svc.Stop()

	log.Println("Planner: Processing trip requests...")
	if err := svc.Start(ctx, "TRIPS"); err != nil && err != context.Canceled {
		log.Printf("Planner: Error: %v", err)
	}
}

// =====================================================
// IMAGER WORKER
// =====================================================

func startImagerWorker(ctx context.Context, js jetstream.JetStream, apiKey, redisAddr string) {
	if apiKey == "" {
		log.Println("Imager: Skipping - no API key")
		return
	}

	imgSvc := images.NewService(redisAddr, apiKey)

	// Wait for stream to be available
	var stream jetstream.Stream
	var err error
	for i := 0; i < 30; i++ {
		stream, err = js.Stream(ctx, "TRIPS")
		if err == nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		log.Printf("Imager: Failed to get stream: %v", err)
		return
	}

	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       "imager-worker-v1",
		FilterSubject: "trips.requested",
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       30 * time.Second,
	})
	if err != nil {
		log.Printf("Imager: Failed to create consumer: %v", err)
		return
	}

	log.Println("Imager: Processing image requests...")

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		msgs, err := cons.Fetch(1, jetstream.FetchMaxWait(5*time.Second))
		if err != nil {
			continue
		}

		for msg := range msgs.Messages() {
			var event types.TripEvent
			if err := json.Unmarshal(msg.Data(), &event); err != nil {
				msg.Ack()
				continue
			}

			reqID := event.Data.RequestID
			dest := event.Data.Destination
			log.Printf("Imager: Fetching images for %s (%s)", reqID, dest)

			imgs, err := imgSvc.GetImages(ctx, dest)
			if err != nil {
				log.Printf("Imager: Fetch failed: %v", err)
				msg.Nak()
				continue
			}

			resEvent := map[string]interface{}{
				"type": "ImagesFound",
				"data": map[string]interface{}{
					"request_id": reqID,
					"images":     imgs,
				},
			}

			data, _ := json.Marshal(resEvent)
			js.Publish(ctx, "trips.images", data)
			msg.Ack()
			log.Printf("Imager: Images found for %s", reqID)
		}
	}
}

// =====================================================
// PLACES WORKER
// =====================================================

func startPlacesWorker(ctx context.Context, js jetstream.JetStream, apiKey, redisAddr string) {
	if apiKey == "" {
		log.Println("Places: Skipping - no API key")
		return
	}

	geoSvc := geo.NewService(apiKey, redisAddr)

	// Wait for stream
	var stream jetstream.Stream
	var err error
	for i := 0; i < 30; i++ {
		stream, err = js.Stream(ctx, "TRIPS")
		if err == nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		log.Printf("Places: Failed to get stream: %v", err)
		return
	}

	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       "places-worker-v1",
		FilterSubject: "trips.requested",
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       30 * time.Second,
	})
	if err != nil {
		log.Printf("Places: Failed to create consumer: %v", err)
		return
	}

	log.Println("Places: Processing place requests...")

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		msgs, err := cons.Fetch(1, jetstream.FetchMaxWait(5*time.Second))
		if err != nil {
			continue
		}

		for msg := range msgs.Messages() {
			var event types.TripEvent
			if err := json.Unmarshal(msg.Data(), &event); err != nil {
				msg.Ack()
				continue
			}

			dest := event.Data.Destination
			reqID := event.Data.RequestID
			log.Printf("Places: Finding places for %s (%s)", reqID, dest)

			lat, lon, err := geoSvc.GetCoordinates(dest)
			if err != nil {
				log.Printf("Places: Geocode failed: %v", err)
				msg.Nak()
				continue
			}

			hotels, _ := geoSvc.SearchPlaces(lat, lon, "accommodation.hotel", 5)
			restaurants, _ := geoSvc.SearchPlaces(lat, lon, "catering.restaurant", 5)
			attractions, _ := geoSvc.SearchPlaces(lat, lon, "tourism.attraction", 5)

			result := map[string]interface{}{
				"type": "PlacesFound",
				"data": map[string]interface{}{
					"request_id": reqID,
					"places": map[string]interface{}{
						"hotels":      hotels,
						"restaurants": restaurants,
						"attractions": attractions,
					},
				},
			}

			data, _ := json.Marshal(result)
			js.Publish(ctx, "trips.places", data)
			msg.Ack()
			log.Printf("Places: Found places for %s", reqID)
		}
	}
}

// =====================================================
// VIBES WORKER
// ====================================================

func startVibesWorker(ctx context.Context, js jetstream.JetStream, apiKey string) {
	if apiKey == "" { return }
	
	go func() {
		vibes.StartWorker(js, apiKey)
	}()
}

// =====================================================
// HTTP ROUTES SETUP
// =====================================================

func (s *Server) setupRoutes() http.Handler {
	rateLimiter := middleware.NewRateLimiter(rate.Limit(10), 20)
	corsConfig := middleware.DefaultCORSConfig()

	mux := http.NewServeMux()

	// Public routes
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/ping", s.handlePing)   // Fast ping (no DB)
	mux.HandleFunc("/health", s.handleHealth) // Full health check (with DB)
	mux.Handle("/metrics", promhttp.Handler())

	// Auth routes
	mux.HandleFunc("/auth/register", s.handleRegister)
	mux.HandleFunc("/auth/login", s.handleLogin)
	mux.HandleFunc("/auth/google/login", s.handleGoogleLogin)
	mux.HandleFunc("/auth/google/callback", s.handleGoogleCallback)
	mux.HandleFunc("/auth/token", s.handleGenerateToken)

	// Protected routes
	mux.Handle("/api/me", s.auth.AuthMiddleware(
		http.HandlerFunc(s.handleGetCurrentUser),
	))
	mux.Handle("/api/trips", s.auth.AuthMiddleware(
		http.HandlerFunc(s.instrumentHandler("/api/trips", s.handleTrips)),
	))
	mux.Handle("/api/trips/", s.auth.AuthMiddleware(
		http.HandlerFunc(s.instrumentHandler("/api/trips/{id}", s.handleTripByID)),
	))

	return middleware.CORS(corsConfig)(rateLimiter.Limit(mux))
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"name":    "Trequila API",
		"version": "1.0.0",
		"docs":    "/health",
	})
}

// =====================================================
// HEALTH CHECK
// =====================================================

// handlePing is a fast endpoint that doesn't touch DB (for uptime monitoring)
func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}

// handleHealth checks DB and NATS connectivity (may be slow if DB is cold)
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	health := map[string]interface{}{
		"status":    "healthy",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"services":  map[string]string{},
	}

	services := health["services"].(map[string]string)

	if err := s.db.Ping(); err != nil {
		services["database"] = "unhealthy"
		health["status"] = "degraded"
	} else {
		services["database"] = "healthy"
	}

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

// =====================================================
// AUTH HANDLERS
// =====================================================

func (s *Server) handleGoogleLogin(w http.ResponseWriter, r *http.Request) {
	state := uuid.NewString()
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

	user, token, err := s.auth.HandleGoogleCallback(r.Context(), code)
	if err != nil {
		log.Printf("Google callback error: %v", err)
		http.Error(w, `{"error":"authentication failed"}`, http.StatusUnauthorized)
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

	if req.UserID == "" {
		req.UserID = uuid.NewString()
	}
	if req.Email == "" {
		req.Email = fmt.Sprintf("dev-%s@trequila.local", req.UserID[:8])
	}
	if req.Name == "" {
		req.Name = "Dev User"
	}

	// Create user in DB
	_, err := s.db.Exec(`
		INSERT INTO users (id, email, name, provider)
		VALUES ($1, $2, $3, 'dev')
		ON CONFLICT (id) DO UPDATE SET name = $3, updated_at = NOW()`,
		req.UserID, req.Email, req.Name,
	)
	if err != nil {
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

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"token":   token,
		"user_id": req.UserID,
		"email":   req.Email,
		"warning": "Development endpoint only",
	})
}

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

// =====================================================
// TRIP HANDLERS
// =====================================================

func (s *Server) instrumentHandler(path string, handler func(http.ResponseWriter, *http.Request)) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		wrapped := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		handler(wrapped, r)
		duration := time.Since(start).Seconds()
		status := strconv.Itoa(wrapped.statusCode)
		metrics.HttpRequestsTotal.WithLabelValues(r.Method, path, status).Inc()
		metrics.HttpRequestDuration.WithLabelValues(r.Method, path).Observe(duration)
	}
}

type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// Flush implements http.Flusher for SSE support
func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

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

func (s *Server) handleTripByID(w http.ResponseWriter, r *http.Request) {
	userID := auth.GetUserIDFromContext(r.Context())
	if userID == "" {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	path := r.URL.Path
	if strings.HasPrefix(path, "/api/trips/stream/") {
		id := strings.TrimPrefix(path, "/api/trips/stream/")
		s.handleStreamTripAuth(w, r, userID, id)
		return
	}

	id := strings.TrimPrefix(path, "/api/trips/")
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

func (s *Server) handleListUserTrips(w http.ResponseWriter, r *http.Request, userID string) {
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

	var total int
	s.db.QueryRow(`SELECT COUNT(*) FROM trips WHERE user_id = $1`, userID).Scan(&total)

	rows, err := s.db.Query(`
		SELECT request_id, source, destination, start_date, end_date, budget, status, created_at
		FROM trips WHERE user_id = $1 ORDER BY created_at DESC LIMIT $2 OFFSET $3`,
		userID, limit, offset,
	)
	if err != nil {
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

	if req.Source == "" || req.Destination == "" || req.StartDate == "" || req.EndDate == "" {
		http.Error(w, `{"error":"missing required fields"}`, http.StatusBadRequest)
		return
	}

	startDate, err := time.Parse("2006-01-02", req.StartDate)
	if err != nil {
		http.Error(w, `{"error":"invalid start_date format"}`, http.StatusBadRequest)
		return
	}
	endDate, err := time.Parse("2006-01-02", req.EndDate)
	if err != nil {
		http.Error(w, `{"error":"invalid end_date format"}`, http.StatusBadRequest)
		return
	}
	if endDate.Before(startDate) {
		http.Error(w, `{"error":"end_date must be after start_date"}`, http.StatusBadRequest)
		return
	}

	if req.Days == 0 {
		req.Days = int(endDate.Sub(startDate).Hours()/24) + 1
	}

	requestID := uuid.NewString()

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

	_, err = s.js.Publish(ctx, "trips.requested", data, jetstream.WithMsgID(requestID))
	if err != nil {
		log.Printf("NATS Publish error: %v", err)
		http.Error(w, `{"error":"failed to queue trip"}`, http.StatusInternalServerError)
		return
	}

	metrics.TripsCreatedTotal.Inc()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"request_id": requestID,
		"status":     "pending",
		"message":    "Trip request accepted",
	})
}

func (s *Server) handleGetUserTrip(w http.ResponseWriter, r *http.Request, userID, tripID string) {
	var status, source, destination, startDate, endDate string
	var budget int
	var itineraryJSON, imagesJSON, placesJSON []byte
	var createdAt, updatedAt time.Time

	err := s.db.QueryRow(`
		SELECT status, source, destination, start_date, end_date, budget,
		       itinerary, images, places, created_at, updated_at
		FROM trips WHERE request_id = $1 AND user_id = $2`,
		tripID, userID,
	).Scan(&status, &source, &destination, &startDate, &endDate, &budget,
		&itineraryJSON, &imagesJSON, &placesJSON, &createdAt, &updatedAt)

	if err == sql.ErrNoRows {
		http.Error(w, `{"error":"trip not found"}`, http.StatusNotFound)
		return
	} else if err != nil {
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

func (s *Server) handleDeleteUserTrip(w http.ResponseWriter, r *http.Request, userID, tripID string) {
	result, err := s.db.Exec(`DELETE FROM trips WHERE request_id = $1 AND user_id = $2`, tripID, userID)
	if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		http.Error(w, `{"error":"trip not found"}`, http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"message": "trip deleted successfully"})
}

// =====================================================
// SSE STREAMING
// =====================================================

func (s *Server) handleStreamTripAuth(w http.ResponseWriter, r *http.Request, userID, tripID string) {
	var exists bool
	s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM trips WHERE request_id = $1 AND user_id = $2)`, tripID, userID).Scan(&exists)
	if !exists {
		http.Error(w, `{"error":"trip not found"}`, http.StatusNotFound)
		return
	}
	s.handleStreamTripInternal(w, r, tripID)
}

func (s *Server) handleStreamTripInternal(w http.ResponseWriter, r *http.Request, id string) {
    var status string
    // Added vibesJSON
    var imagesJSON, placesJSON, itineraryJSON, vibesJSON []byte 
    
    // 1. UPDATE QUERY to include 'vibes'
    err := s.db.QueryRow(`
        SELECT status, images, places, itinerary, vibes 
        FROM trips WHERE request_id = $1`, id,
    ).Scan(&status, &imagesJSON, &placesJSON, &itineraryJSON, &vibesJSON)

    if err == sql.ErrNoRows {
        http.Error(w, "trip not found", http.StatusNotFound)
        return
    } else if err != nil {
        http.Error(w, "server error", http.StatusInternalServerError)
        return
    }

    // ... headers setup (Content-Type, etc.) ...
    w.Header().Set("Content-Type", "text/event-stream")
    w.Header().Set("Cache-Control", "no-cache")
    w.Header().Set("Connection", "keep-alive")
    w.Header().Set("Access-Control-Allow-Origin", "*")

    flusher, ok := w.(http.Flusher)
    if !ok {
        http.Error(w, "streaming not supported", http.StatusInternalServerError)
        return
    }

    fmt.Fprintf(w, "event: connected\ndata: {\"request_id\":\"%s\",\"message\":\"Stream connected\"}\n\n", id)
    flusher.Flush()

    metrics.SSEConnectionsActive.Inc()
    defer metrics.SSEConnectionsActive.Dec()

    // 2. CHECK FLAGS
    receivedImages := len(imagesJSON) > 0
    receivedPlaces := len(placesJSON) > 0
    receivedItinerary := len(itineraryJSON) > 0
    receivedVibes := len(vibesJSON) > 0 // New flag

    // 3. SEND DB DATA
    if receivedImages {
        data, _ := json.Marshal(map[string]interface{}{"type": "images", "images": json.RawMessage(imagesJSON)})
        fmt.Fprintf(w, "event: images\ndata: %s\n\n", data)
        flusher.Flush()
    }
    if receivedPlaces {
        data, _ := json.Marshal(map[string]interface{}{"type": "places", "places": json.RawMessage(placesJSON)})
        fmt.Fprintf(w, "event: places\ndata: %s\n\n", data)
        flusher.Flush()
    }
    // New Vibes Block
    if receivedVibes {
        data, _ := json.Marshal(map[string]interface{}{"type": "vibes", "vibes": json.RawMessage(vibesJSON)})
        fmt.Fprintf(w, "event: vibes\ndata: %s\n\n", data)
        flusher.Flush()
    }
    if receivedItinerary {
        data, _ := json.Marshal(map[string]interface{}{"type": "itinerary", "itinerary": json.RawMessage(itineraryJSON)})
        fmt.Fprintf(w, "event: itinerary\ndata: %s\n\n", data)
        flusher.Flush()
    }

    // 4. CHECK COMPLETION (Both must be present to stop immediately)
    if receivedItinerary && receivedVibes {
        fmt.Fprintf(w, "event: complete\ndata: {\"message\":\"All data received\"}\n\n")
        flusher.Flush()
        return
    }

    // ... Subscription setup ...
    msgChan := make(chan *nats.Msg, 64)
    // Optimization: Subscribe to specific ID to reduce CPU load
    sub, err := s.nc.ChanSubscribe("trips.>", msgChan) 
    if err != nil { return }
    defer sub.Unsubscribe()

    ctx := r.Context()
    timeout := time.After(2 * time.Minute)

    for {
        select {
        case <-ctx.Done():
            return
        case <-timeout:
            fmt.Fprintf(w, "event: timeout\ndata: {\"message\":\"Stream timeout\"}\n\n")
            flusher.Flush()
            return
        case msg := <-msgChan:
            var envelope struct {
                Type string `json:"type"`
                Data struct {
                    RequestID string          `json:"request_id"`
                    Images    json.RawMessage `json:"images,omitempty"`
                    Places    json.RawMessage `json:"places,omitempty"`
                    Itinerary json.RawMessage `json:"itinerary,omitempty"`
                    Vibes     json.RawMessage `json:"vibes,omitempty"` // Add Vibes
                } `json:"data"`
            }

            if err := json.Unmarshal(msg.Data, &envelope); err != nil { continue }
            if envelope.Data.RequestID != id { continue }

            switch msg.Subject {
            case "trips.images":
                if !receivedImages {
                    receivedImages = true
                    data, _ := json.Marshal(map[string]interface{}{"type": "images", "images": envelope.Data.Images})
                    fmt.Fprintf(w, "event: images\ndata: %s\n\n", data)
                    flusher.Flush()
                }
            case "trips.places":
                if !receivedPlaces {
                    receivedPlaces = true
                    data, _ := json.Marshal(map[string]interface{}{"type": "places", "places": envelope.Data.Places})
                    fmt.Fprintf(w, "event: places\ndata: %s\n\n", data)
                    flusher.Flush()
                }
            
            // 5. HANDLE VIBES EVENT
            case "trips.vibes":
                if !receivedVibes {
                    receivedVibes = true
                    data, _ := json.Marshal(map[string]interface{}{"type": "vibes", "vibes": envelope.Data.Vibes})
                    fmt.Fprintf(w, "event: vibes\ndata: %s\n\n", data)
                    flusher.Flush()
                }
                // Check if we can close now
                if receivedItinerary && receivedVibes {
                    fmt.Fprintf(w, "event: complete\ndata: {\"message\":\"All data received\"}\n\n")
                    flusher.Flush()
                    return
                }

            case "trips.itinerary":
                if !receivedItinerary {
                    receivedItinerary = true
                    data, _ := json.Marshal(map[string]interface{}{"type": "itinerary", "itinerary": envelope.Data.Itinerary})
                    fmt.Fprintf(w, "event: itinerary\ndata: %s\n\n", data)
                    flusher.Flush()
                }
                // Check if we can close now (wait for vibes too)
                if receivedItinerary && receivedVibes {
                    fmt.Fprintf(w, "event: complete\ndata: {\"message\":\"All data received\"}\n\n")
                    flusher.Flush()
                    return
                }
            }
        }
    }
}

// =====================================================
// BACKGROUND CONSUMERS (Save to DB)
// =====================================================

func (s *Server) consumeItineraries(ctx context.Context) {
	var cons jetstream.Consumer
	for {
		stream, err := s.js.Stream(ctx, "TRIPS")
		if err == nil {
			cons, err = stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
				Durable:       "api-itinerary-writer-v1",
				FilterSubject: "trips.itinerary",
				AckPolicy:     jetstream.AckExplicitPolicy,
				AckWait:       30 * time.Second,
			})
			if err == nil {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		msgs, _ := cons.Fetch(1, jetstream.FetchMaxWait(5*time.Second))
		for msg := range msgs.Messages() {
			var event types.ItineraryGeneratedEvent
			if err := json.Unmarshal(msg.Data(), &event); err != nil {
				msg.Ack()
				continue
			}

			itineraryBytes, _ := json.Marshal(event.Data.Itinerary)
			_, err := s.db.Exec(`
				UPDATE trips SET status = 'completed', itinerary = $1, updated_at = NOW()
				WHERE request_id = $2`,
				string(itineraryBytes), event.Data.RequestID,
			)
			if err != nil {
				msg.Nak()
			} else {
				log.Printf("Itinerary saved for %s", event.Data.RequestID)
				msg.Ack()
			}
		}
	}
}

func (s *Server) consumeImages(ctx context.Context) {
	var cons jetstream.Consumer
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
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}

	type ImagePayload struct {
		Data struct {
			RequestID string          `json:"request_id"`
			Images    json.RawMessage `json:"images"`
		} `json:"data"`
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		msgs, _ := cons.Fetch(1, jetstream.FetchMaxWait(5*time.Second))
		for msg := range msgs.Messages() {
			var event ImagePayload
			if err := json.Unmarshal(msg.Data(), &event); err != nil {
				msg.Ack()
				continue
			}

			_, err := s.db.Exec(`UPDATE trips SET images = $1 WHERE request_id = $2`,
				string(event.Data.Images), event.Data.RequestID)
			if err != nil {
				msg.Nak()
			} else {
				log.Printf("Images saved for %s", event.Data.RequestID)
				msg.Ack()
			}
		}
	}
}

func (s *Server) consumePlaces(ctx context.Context) {
	var cons jetstream.Consumer
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
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}

	type PlacesPayload struct {
		Data struct {
			RequestID string          `json:"request_id"`
			Places    json.RawMessage `json:"places"`
		} `json:"data"`
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		msgs, _ := cons.Fetch(1, jetstream.FetchMaxWait(5*time.Second))
		for msg := range msgs.Messages() {
			var event PlacesPayload
			if err := json.Unmarshal(msg.Data(), &event); err != nil {
				msg.Ack()
				continue
			}

			_, err := s.db.Exec(`UPDATE trips SET places = $1 WHERE request_id = $2`,
				string(event.Data.Places), event.Data.RequestID)
			if err != nil {
				msg.Nak()
			} else {
				log.Printf("Places saved for %s", event.Data.RequestID)
				msg.Ack()
			}
		}
	}
}


// Add this to your main.go or internal/server/consumers.go
func (s *Server) consumeVibes(ctx context.Context) {
    // 1. Setup Consumer
    var cons jetstream.Consumer
    for {
        stream, err := s.js.Stream(ctx, "TRIPS")
        if err == nil {
            cons, err = stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
                Durable:       "api-vibes-writer-v1",
                FilterSubject: "trips.vibes",
                AckPolicy:     jetstream.AckExplicitPolicy,
            })
            if err == nil { break }
        }
        time.Sleep(2 * time.Second)
    }

    type VibesPayload struct {
        Data struct {
            RequestID string          `json:"request_id"`
            Vibes     json.RawMessage `json:"vibes"` // Raw JSONB
        } `json:"data"`
    }

    log.Println("Started Vibes DB Consumer")

    // 2. Consume Loop
    for {
        select {
        case <-ctx.Done():
            return
        default:
        }

        msgs, err := cons.Fetch(1, jetstream.FetchMaxWait(5*time.Second))
        if err != nil { continue }

        for msg := range msgs.Messages() {
            var event VibesPayload
            if err := json.Unmarshal(msg.Data(), &event); err != nil {
                msg.Ack()
                continue
            }

            // 3. Save to Postgres
            // Requires: ALTER TABLE trips ADD COLUMN vibes JSONB;
            _, err := s.db.Exec(`UPDATE trips SET vibes = $1, updated_at = NOW() WHERE request_id = $2`,
                string(event.Data.Vibes), event.Data.RequestID)
            
            if err != nil {
                log.Printf("Failed to save vibes to DB: %v", err)
                msg.Nak()
            } else {
                log.Printf("Vibes saved to DB for %s", event.Data.RequestID)
                msg.Ack()
            }
        }
    }
}