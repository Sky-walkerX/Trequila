// cmd/monolith/main.go
// Monolith service - combines API, Planner, Imager, and Places into a single binary
// Uses external NATS for event-driven communication between goroutines
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	"github.com/sky-walkerx/trequila/internal/auth"
	"github.com/sky-walkerx/trequila/internal/chatbot"
	"github.com/sky-walkerx/trequila/internal/crypto"
	"github.com/sky-walkerx/trequila/internal/geo"
	"github.com/sky-walkerx/trequila/internal/images"
	"github.com/sky-walkerx/trequila/internal/llm"
	trequilamcp "github.com/sky-walkerx/trequila/internal/mcp"
	"github.com/sky-walkerx/trequila/internal/metrics"
	"github.com/sky-walkerx/trequila/internal/middleware"
	natshelper "github.com/sky-walkerx/trequila/internal/nats"
	"github.com/sky-walkerx/trequila/internal/planner"
	"github.com/sky-walkerx/trequila/internal/providerkeys"
	"github.com/sky-walkerx/trequila/internal/route"
	"github.com/sky-walkerx/trequila/internal/types"
	"golang.org/x/time/rate"
)

// Server holds shared dependencies
type Server struct {
	js             jetstream.JetStream
	nc             *nats.Conn
	db             *sql.DB
	auth           *auth.AuthService
	keys           *providerkeys.Store
	mcpServer      *trequilamcp.Server
	chatbotHandler *chatbot.Handler
	frontendURL    string
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
	cipherKey, err := crypto.NewCipherFromBase64(config.EncryptionKey)
	if err != nil {
		log.Fatalf("Invalid ENCRYPTION_KEY: %v", err)
	}

	server := &Server{
		js:          js,
		nc:          nc,
		db:          db,
		auth:        auth.NewAuthService(db),
		keys:        providerkeys.NewStore(db, cipherKey),
		frontendURL: config.FrontendURL,
	}

	geoSvc := geo.NewService(config.RedisAddr)
	imgSvc := images.NewService(config.RedisAddr)
	server.mcpServer = trequilamcp.NewServer(db, js, nc, server.keys, geoSvc, imgSvc, server.auth)

	chatbotStore := chatbot.NewStore(db)
	server.chatbotHandler = chatbot.NewHandler(chatbotStore, server.keys, server.mcpServer)

	// 5. Start background workers (microservices as goroutines)

	// API consumers (save results to DB)
	go server.consumeItineraries(ctx)
	go server.consumeImages(ctx)
	go server.consumePlaces(ctx)
	go server.consumeRoutes(ctx)
	go server.consumeFailures(ctx)
	log.Println("Started API consumers")

	// Planner worker (AI itinerary generation)
	go startPlannerWorker(ctx, js, server.keys)
	log.Println("Started Planner worker")

	// Imager worker (fetch destination images)
	go startImagerWorker(ctx, js, config.RedisAddr)
	log.Println("Started Imager worker")

	// Places worker (fetch POIs)
	go startPlacesWorker(ctx, js, config.RedisAddr)
	log.Println("Started Places worker")

	// Route worker (per-day stop ordering). It reuses geoSvc so its geocoding
	// shares the same Redis cache the places worker already fills.
	go route.StartWorker(ctx, js, geoSvc)
	log.Println("Started Route worker")

	// 6. Setup HTTP server
	handler := server.setupRoutes()

	port := config.Port
	srv := &http.Server{
		Addr:        ":" + port,
		Handler:     handler,
		ReadTimeout: 15 * time.Second,
		// No WriteTimeout: it is absolute from the start of the response, so any
		// value caps how long an SSE stream may run. The streaming handlers
		// (trip stream, chat) each enforce their own deadline and
		// send heartbeats, and non-streaming handlers return promptly.
		WriteTimeout:      0,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Start HTTP server
	go func() {
		log.Printf("HTTP server listening on :%s", port)
		log.Println("=== Trequila Monolith Ready ===")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// Keep-alive: prevent Heroku dyno from sleeping (pings own health endpoint every 25 min)
	if os.Getenv("ENABLE_KEEP_ALIVE") == "true" {
		go func() {
			ticker := time.NewTicker(25 * time.Minute)
			defer ticker.Stop()
			pingURL := fmt.Sprintf("http://localhost:%s/ping", port)
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					resp, err := http.Get(pingURL)
					if err != nil {
						log.Printf("Keep-alive ping failed: %v", err)
					} else {
						resp.Body.Close()
						log.Println("Keep-alive ping OK")
					}
				}
			}
		}()
		log.Println("Keep-alive enabled (25 min interval)")
	}

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
	Port          string
	NatsURL       string
	NatsCredsFile string
	NatsCreds     string // Inline credentials content (for Heroku)
	DatabaseURL   string
	RedisAddr     string
	EncryptionKey string
	FrontendURL   string // Where OAuth callbacks redirect the browser back to
}

func loadConfig() Config {
	// JWT_SECRET is validated (and enforced) in auth.NewAuthService.
	// LLM keys are per-user (BYOK) and live in the provider_keys table.
	return Config{
		Port:          getEnv("PORT", "8080"),
		NatsURL:       getEnv("NATS_URL", nats.DefaultURL),
		NatsCredsFile: os.Getenv("NATS_CREDS_FILE"), // Path to .creds file (local)
		NatsCreds:     os.Getenv("NATS_CREDS"),      // Inline creds content (Heroku)
		DatabaseURL:   getEnv("DATABASE_URL", getEnv("DB_URL", "postgres://postgres:password@localhost:5432/trequila?sslmode=disable")),
		RedisAddr:     getEnv("REDIS_URL", getEnv("REDIS_ADDR", "localhost:6379")),
		EncryptionKey: os.Getenv("ENCRYPTION_KEY"),
		FrontendURL:   strings.TrimRight(getEnv("FRONTEND_URL", middleware.DefaultAllowedOrigin), "/"),
	}
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

func startPlannerWorker(ctx context.Context, js jetstream.JetStream, keys *providerkeys.Store) {
	// Create planner service
	plannerConfig := planner.DefaultConfig()
	svc, err := planner.New(js, keys, plannerConfig)
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

func startImagerWorker(ctx context.Context, js jetstream.JetStream, redisAddr string) {
	imgSvc := images.NewService(redisAddr)

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
					"user_id":    event.Data.UserID,
					"provider":   event.Data.Provider,
					"model":      event.Data.Model,
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

func startPlacesWorker(ctx context.Context, js jetstream.JetStream, redisAddr string) {
	geoSvc := geo.NewService(redisAddr)

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
		Durable:       "places-worker-v2",
		FilterSubject: "trips.requested",
		AckPolicy:     jetstream.AckExplicitPolicy,
		// Three sequential Overpass queries against a dense city routinely take
		// over a minute; a short AckWait redelivers the request mid-flight.
		AckWait:    5 * time.Minute,
		MaxDeliver: 3,
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
				// Check if it's a "Not Found" error (Terminal Error)
				if strings.Contains(err.Error(), "not found") {
					log.Printf("Places: Invalid location '%s', dropping request.", dest)
					msg.Ack()
					continue
				}

				// If it's a different error (e.g., Network / Redis down), retry later
				log.Printf("Places: Transient error for '%s': %v", dest, err)
				msg.Nak()
				continue
			}

			// Each category is fetched independently; a failure in one must not
			// silently become a `null` column, and a failure in *all* of them
			// leaves the trip with no places at all, so retry instead.
			search := func(category string) ([]geo.Place, bool) {
				places, err := geoSvc.SearchPlaces(lat, lon, category, 5)
				if err != nil {
					log.Printf("Places: %s lookup failed for %s: %v", category, dest, err)
					return []geo.Place{}, false
				}
				if len(places) == 0 {
					log.Printf("Places: %s lookup returned nothing for %s", category, dest)
				}
				return places, true
			}

			hotels, okHotels := search("accommodation.hotel")
			restaurants, okRestaurants := search("catering.restaurant")
			attractions, okAttractions := search("tourism.attraction")

			if !okHotels && !okRestaurants && !okAttractions {
				log.Printf("Places: all lookups failed for %s (%s), retrying later", reqID, dest)
				msg.Nak()
				continue
			}
			if len(hotels)+len(restaurants)+len(attractions) == 0 {
				log.Printf("Places: no places found near %s (%s)", reqID, dest)
			}

			result := map[string]interface{}{
				"type": "PlacesFound",
				"data": map[string]interface{}{
					"request_id": reqID,
					"user_id":    event.Data.UserID,
					"provider":   event.Data.Provider,
					"model":      event.Data.Model,
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
// HTTP ROUTES SETUP
// =====================================================

func (s *Server) setupRoutes() http.Handler {
	rateLimiter := middleware.NewRateLimiter(rate.Limit(10), 20)
	corsConfig := middleware.DefaultCORSConfig()

	mux := http.NewServeMux()

	// Public routes
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/ping", s.handlePing)     // Fast ping (no DB)
	mux.HandleFunc("/health", s.handleHealth) // Full health check (with DB)
	mux.Handle("/metrics", promhttp.Handler())

	// Auth routes
	mux.HandleFunc("/auth/register", s.handleRegister)
	mux.HandleFunc("/auth/login", s.handleLogin)
	mux.HandleFunc("/auth/google/login", s.handleGoogleLogin)
	mux.HandleFunc("/auth/google/callback", s.handleGoogleCallback)

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

	// Keys endpoints
	mux.Handle("/api/keys", s.auth.AuthMiddleware(http.HandlerFunc(s.handleKeys)))
	mux.Handle("/api/keys/test", s.auth.AuthMiddleware(http.HandlerFunc(s.handleKeysTest)))
	mux.Handle("/api/keys/", s.auth.AuthMiddleware(http.HandlerFunc(s.handleKeyByProvider)))

	// Feature: Chatbot Agentic Endpoint
	mux.Handle("/api/chat", s.auth.AuthMiddleware(s.chatbotHandler))

	// Feature: Weather Replan
	mux.Handle("/api/trips/replan/", s.auth.AuthMiddleware(
		http.HandlerFunc(s.instrumentHandler("/api/trips/replan", s.handleWeatherReplan)),
	))

	// Feature: Route optimisation. More specific than the "/api/trips/" pattern,
	// so ServeMux routes it here rather than to handleTripByID.
	mux.Handle("/api/trips/optimize/", s.auth.AuthMiddleware(
		http.HandlerFunc(s.instrumentHandler("/api/trips/optimize", s.handleOptimizeTripRequest)),
	))

	// MCP SSE Endpoint
	mux.Handle("/mcp", s.auth.AuthMiddleware(s.mcpServer.Handler()))

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

	http.SetCookie(w, &http.Cookie{
		Name:     "oauth_state",
		Value:    state,
		Path:     "/",
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   600,
	})

	url := s.auth.GoogleLoginURL(state)
	if url == "" {
		http.Error(w, `{"error":"google oauth not configured"}`, http.StatusNotImplemented)
		return
	}
	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

func (s *Server) handleGoogleCallback(w http.ResponseWriter, r *http.Request) {
	stateCookie, err := r.Cookie("oauth_state")
	if err != nil || stateCookie.Value != r.URL.Query().Get("state") {
		http.Error(w, "invalid oauth state", http.StatusBadRequest)
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code parameter", http.StatusBadRequest)
		return
	}

	_, token, err := s.auth.HandleGoogleCallback(r.Context(), code)
	if err != nil {
		http.Redirect(w, r, s.frontendURL+"/auth?error=auth_failed", http.StatusTemporaryRedirect)
		return
	}

	// Cookie for same-site callers (it is never sent cross-site, and the SPA
	// cannot read it), plus the token in the URL *fragment*, which the SPA reads
	// on load. A fragment is never sent to a server, so it stays out of access
	// logs and Referer headers the way a ?token= query param would not.
	http.SetCookie(w, &http.Cookie{
		Name:     "auth_token",
		Value:    token,
		Path:     "/",
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   86400, // 24 hours
	})

	http.Redirect(w, r, s.frontendURL+"/#token="+token, http.StatusTemporaryRedirect)
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
		Provider    string   `json:"provider"`
		Model       string   `json:"model"`
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

	cfg, err := s.keys.Resolve(r.Context(), userID, req.Provider)
	if err != nil {
		if errors.Is(err, providerkeys.ErrNoKey) {
			http.Error(w, `{"error":"add an API key in Settings"}`, http.StatusBadRequest)
		} else {
			http.Error(w, `{"error":"failed to resolve API key"}`, http.StatusInternalServerError)
		}
		return
	}
	if req.Provider == "" {
		req.Provider = string(cfg.Kind)
	}
	if req.Model == "" {
		req.Model = cfg.Model
	}

	requestID := uuid.NewString()

	_, err = s.db.Exec(`
		INSERT INTO trips (request_id, user_id, source, destination, start_date, end_date, budget, status, provider, model)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'pending', $8, $9)`,
		requestID, userID, req.Source, req.Destination, req.StartDate, req.EndDate, req.Budget, req.Provider, req.Model,
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
			Provider:    req.Provider,
			Model:       req.Model,
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
	var itineraryJSON, imagesJSON, placesJSON, routeJSON []byte
	var createdAt, updatedAt time.Time

	err := s.db.QueryRow(`
		SELECT t.status, t.source, t.destination, t.start_date, t.end_date, t.budget,
		       t.itinerary, t.images, t.places, t.route, t.created_at, t.updated_at
		FROM trips t
		WHERE t.request_id = $1 AND t.user_id = $2`,
		tripID, userID,
	).Scan(&status, &source, &destination, &startDate, &endDate, &budget,
		&itineraryJSON, &imagesJSON, &placesJSON, &routeJSON, &createdAt, &updatedAt)

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
	if len(routeJSON) > 0 {
		resp["route"] = json.RawMessage(routeJSON)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleOptimizeTripRequest recomputes a trip's route. Ordering a long trip
// means dozens of rate-limited geocoder lookups, far past Heroku's 30-second
// request cap, so this only enqueues the work; the client polls
// GET /api/trips/{id} and watches route.generated_at change.
func (s *Server) handleOptimizeTripRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	userID := auth.GetUserIDFromContext(r.Context())
	if userID == "" {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	tripID := strings.TrimPrefix(r.URL.Path, "/api/trips/optimize/")
	if tripID == "" {
		http.Error(w, `{"error":"trip id required"}`, http.StatusBadRequest)
		return
	}

	var destination string
	var itineraryJSON []byte
	err := s.db.QueryRow(`
		SELECT t.destination, t.itinerary
		FROM trips t
		WHERE t.request_id = $1 AND t.user_id = $2`,
		tripID, userID,
	).Scan(&destination, &itineraryJSON)

	if err == sql.ErrNoRows {
		http.Error(w, `{"error":"trip not found"}`, http.StatusNotFound)
		return
	} else if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}

	var itinerary []types.DayPlan
	if err := json.Unmarshal(itineraryJSON, &itinerary); err != nil || len(itinerary) == 0 {
		http.Error(w, `{"error":"trip has no itinerary to optimize"}`, http.StatusConflict)
		return
	}

	s.requestRoute(r.Context(), tripID, destination, itinerary)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{
		"status":     "optimizing",
		"request_id": tripID,
	})
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

const (
	// streamHeartbeatInterval must stay well under the ~55s idle window enforced
	// by Heroku's router (error H15) and most reverse proxies.
	streamHeartbeatInterval = 20 * time.Second
	// streamHardTimeout caps a single SSE connection. Overpass on a cold cache
	// regularly exceeds two minutes.
	streamHardTimeout = 5 * time.Minute
	// streamPlacesGrace is how long to keep streaming after the itinerary lands,
	// waiting for places, before declaring the trip complete with partial data.
	streamPlacesGrace = 30 * time.Second
)

func (s *Server) handleStreamTripAuth(w http.ResponseWriter, r *http.Request, userID, tripID string) {
	var exists bool
	s.db.QueryRow(`SELECT EXISTS(
		SELECT 1 FROM trips WHERE request_id = $1 AND user_id = $2
	)`, tripID, userID).Scan(&exists)
	if !exists {
		http.Error(w, `{"error":"trip not found"}`, http.StatusNotFound)
		return
	}
	s.handleStreamTripInternal(w, r, tripID)
}

func (s *Server) handleStreamTripInternal(w http.ResponseWriter, r *http.Request, id string) {
	var status, errorReason string
	var imagesJSON, placesJSON, itineraryJSON, routeJSON []byte

	err := s.db.QueryRow(`
        SELECT status, images, places, itinerary, route, COALESCE(error_reason, '')
        FROM trips WHERE request_id = $1`, id,
	).Scan(&status, &imagesJSON, &placesJSON, &itineraryJSON, &routeJSON, &errorReason)

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
	progress := streamProgress{
		Itinerary: len(itineraryJSON) > 0,
		Places:    len(placesJSON) > 0,
	}

	// 3. SEND DB DATA
	if receivedImages {
		data, _ := json.Marshal(map[string]interface{}{"type": "images", "images": json.RawMessage(imagesJSON)})
		fmt.Fprintf(w, "event: images\ndata: %s\n\n", data)
		flusher.Flush()
	}
	if progress.Places {
		data, _ := json.Marshal(map[string]interface{}{"type": "places", "places": json.RawMessage(placesJSON)})
		fmt.Fprintf(w, "event: places\ndata: %s\n\n", data)
		flusher.Flush()
	}
	if progress.Itinerary {
		data, _ := json.Marshal(map[string]interface{}{"type": "itinerary", "itinerary": json.RawMessage(itineraryJSON)})
		fmt.Fprintf(w, "event: itinerary\ndata: %s\n\n", data)
		flusher.Flush()
	}
	if len(routeJSON) > 0 {
		data, _ := json.Marshal(map[string]interface{}{"type": "route", "route": json.RawMessage(routeJSON)})
		fmt.Fprintf(w, "event: route\ndata: %s\n\n", data)
		flusher.Flush()
	}

	// 4. CHECK COMPLETION OR FAILURE
	if status == "failed" {
		data, _ := json.Marshal(map[string]interface{}{"type": "failed", "reason": errorReason})
		fmt.Fprintf(w, "event: failed\ndata: %s\n\n", data)
		flusher.Flush()
		return
	}

	if progress.Complete() {
		fmt.Fprintf(w, "event: complete\ndata: {\"message\":\"All data received\"}\n\n")
		flusher.Flush()
		return
	}

	// ... Subscription setup ...
	msgChan := make(chan *nats.Msg, 64)
	sub, err := s.nc.ChanSubscribe("trips.>", msgChan)
	if err != nil {
		// Returning silently here leaves the client watching a stream that will
		// never carry another event; it has to be told the run is not coming.
		log.Printf("Stream: subscribe failed for %s: %v", id, err)
		data, _ := json.Marshal(map[string]interface{}{
			"type":   "failed",
			"reason": "The planning server could not subscribe to trip updates. Please try again.",
		})
		fmt.Fprintf(w, "event: failed\ndata: %s\n\n", data)
		flusher.Flush()
		return
	}
	defer sub.Unsubscribe()

	ctx := r.Context()
	timeout := time.After(streamHardTimeout)

	// Grace period: once the itinerary exists, wait a bounded time for places.
	// The itinerary may already have been replayed from the DB above, in which
	// case no live trips.itinerary event will ever arrive to start the clock.
	var placesGrace <-chan time.Time
	if progress.GraceDue() {
		placesGrace = time.After(streamPlacesGrace)
	}

	// Stages can be more than a minute apart. Proxies (Heroku's router
	// among them) drop a response that sends nothing for ~55s, so keep the
	// connection alive with SSE comments between real events.
	heartbeat := time.NewTicker(streamHeartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timeout:
			fmt.Fprintf(w, "event: timeout\ndata: {\"message\":\"Stream timeout\"}\n\n")
			flusher.Flush()
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case <-placesGrace:
			// Places grace period expired — complete with partial data
			fmt.Fprintf(w, "event: complete\ndata: {\"message\":\"All data received\"}\n\n")
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
					Route     json.RawMessage `json:"route,omitempty"`
					Reason    string          `json:"reason,omitempty"`
				} `json:"data"`
			}

			if err := json.Unmarshal(msg.Data, &envelope); err != nil {
				continue
			}
			if envelope.Data.RequestID != id {
				continue
			}

			switch msg.Subject {
			case "trips.images":
				if !receivedImages {
					receivedImages = true
					data, _ := json.Marshal(map[string]interface{}{"type": "images", "images": envelope.Data.Images})
					fmt.Fprintf(w, "event: images\ndata: %s\n\n", data)
					flusher.Flush()
				}
			case "trips.places":
				if !progress.Places {
					progress.Places = true
					data, _ := json.Marshal(map[string]interface{}{"type": "places", "places": envelope.Data.Places})
					fmt.Fprintf(w, "event: places\ndata: %s\n\n", data)
					flusher.Flush()
				}
				if progress.Complete() {
					fmt.Fprintf(w, "event: complete\ndata: {\"message\":\"All data received\"}\n\n")
					flusher.Flush()
					return
				}

			case "trips.itinerary":
				if !progress.Itinerary {
					progress.Itinerary = true
					data, _ := json.Marshal(map[string]interface{}{"type": "itinerary", "itinerary": envelope.Data.Itinerary})
					fmt.Fprintf(w, "event: itinerary\ndata: %s\n\n", data)
					flusher.Flush()
				}
				if progress.Complete() {
					fmt.Fprintf(w, "event: complete\ndata: {\"message\":\"All data received\"}\n\n")
					flusher.Flush()
					return
				}
				// Places is the last stage that can lag; give it a bounded window
				// rather than holding the stream open to the hard timeout.
				if placesGrace == nil {
					placesGrace = time.After(streamPlacesGrace)
				}
			case route.SubjectResult:
				// Forwarded opportunistically. Routing starts only once the
				// itinerary is stored and then geocodes at about one lookup a
				// second, so it often lands after the stream has already
				// completed — the trip GET serves it in that case. Completion is
				// deliberately not gated on it.
				data, _ := json.Marshal(map[string]interface{}{"type": "route", "route": envelope.Data.Route})
				fmt.Fprintf(w, "event: route\ndata: %s\n\n", data)
				flusher.Flush()

			case "trips.failed":
				data, _ := json.Marshal(map[string]interface{}{"type": "failed", "reason": envelope.Data.Reason})
				fmt.Fprintf(w, "event: failed\ndata: %s\n\n", data)
				flusher.Flush()
				return
			}
		}
	}
}

// =====================================================
// FEATURE: WEATHER-ADAPTIVE REPLANNING
// =====================================================

func (s *Server) handleWeatherReplan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	userID := auth.GetUserIDFromContext(r.Context())
	if userID == "" {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	tripID := strings.TrimPrefix(r.URL.Path, "/api/trips/replan/")
	if tripID == "" {
		http.Error(w, `{"error":"trip id required"}`, http.StatusBadRequest)
		return
	}

	// Get existing trip data
	var destination, provider, model string
	var itineraryJSON []byte

	err := s.db.QueryRow(`
		SELECT destination, itinerary, COALESCE(provider, ''), COALESCE(model, '')
		FROM trips WHERE request_id = $1 AND user_id = $2 AND itinerary IS NOT NULL`,
		tripID, userID,
	).Scan(&destination, &itineraryJSON, &provider, &model)

	if err == sql.ErrNoRows {
		http.Error(w, `{"error":"trip not found"}`, http.StatusNotFound)
		return
	} else if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}

	var itinerary []types.DayPlan
	if len(itineraryJSON) > 0 {
		json.Unmarshal(itineraryJSON, &itinerary)
	}

	if len(itinerary) == 0 {
		http.Error(w, `{"error":"no itinerary generated yet"}`, http.StatusBadRequest)
		return
	}

	// Parse optional request body for specific day index
	var req struct {
		DayIndex *int `json:"day_index"` // nil = replan all rainy days
	}
	json.NewDecoder(r.Body).Decode(&req)

	// Fetch weather forecast for the destination
	// Use geocoding to get lat/lon (we'll use a simple approach)
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	// Identify rainy days using Open-Meteo forecast
	forecastURL := fmt.Sprintf(
		"https://api.open-meteo.com/v1/forecast?latitude=0&longitude=0&daily=weathercode,temperature_2m_max&timezone=auto&forecast_days=%d",
		len(itinerary),
	)
	// We need lat/lon — fetch from geocoding
	geoURL := fmt.Sprintf("https://geocoding-api.open-meteo.com/v1/search?name=%s&count=1", strings.ReplaceAll(destination, " ", "+"))
	geoResp, err := http.Get(geoURL)
	if err == nil {
		defer geoResp.Body.Close()
		var geoData struct {
			Results []struct {
				Lat float64 `json:"latitude"`
				Lon float64 `json:"longitude"`
			} `json:"results"`
		}
		if json.NewDecoder(geoResp.Body).Decode(&geoData) == nil && len(geoData.Results) > 0 {
			forecastURL = fmt.Sprintf(
				"https://api.open-meteo.com/v1/forecast?latitude=%.4f&longitude=%.4f&daily=weathercode,temperature_2m_max&timezone=auto&forecast_days=%d",
				geoData.Results[0].Lat, geoData.Results[0].Lon, len(itinerary),
			)
		}
	}

	// Fetch weather forecast
	type weatherForecast struct {
		Daily struct {
			WeatherCode []int     `json:"weathercode"`
			MaxTemp     []float64 `json:"temperature_2m_max"`
		} `json:"daily"`
	}

	var forecast weatherForecast
	wResp, err := http.Get(forecastURL)
	if err == nil {
		defer wResp.Body.Close()
		json.NewDecoder(wResp.Body).Decode(&forecast)
	}

	// Determine which days need replanning
	rainyDays := []int{} // indices into itinerary
	weatherInfo := map[int]string{}
	for i, code := range forecast.Daily.WeatherCode {
		if i >= len(itinerary) {
			break
		}
		if code >= 51 { // Rain/Storm codes per WMO
			rainyDays = append(rainyDays, i)
			temp := 0.0
			if i < len(forecast.Daily.MaxTemp) {
				temp = forecast.Daily.MaxTemp[i]
			}
			emoji := "🌧️"
			if code >= 95 {
				emoji = "⛈️"
			}
			weatherInfo[i] = fmt.Sprintf("%.0f°C %s", temp, emoji)
		}
	}

	// If specific day requested, only replan that day
	if req.DayIndex != nil {
		found := false
		for _, d := range rainyDays {
			if d == *req.DayIndex {
				found = true
				break
			}
		}
		if !found {
			// User wants to force-replan a specific day regardless of weather
			rainyDays = []int{*req.DayIndex}
			weatherInfo[*req.DayIndex] = "User requested replan"
		} else {
			rainyDays = []int{*req.DayIndex}
		}
	}

	if len(rainyDays) == 0 {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"message":    "No rainy days detected — itinerary looks good!",
			"rainy_days": []int{},
			"replanned":  false,
			"itinerary":  itinerary,
		})
		return
	}

	cfg, err := s.keys.Resolve(ctx, userID, provider)
	if err != nil {
		if errors.Is(err, providerkeys.ErrNoKey) {
			http.Error(w, `{"error":"add an API key in Settings"}`, http.StatusBadRequest)
		} else {
			http.Error(w, `{"error":"failed to resolve API key"}`, http.StatusInternalServerError)
		}
		return
	}
	if model != "" {
		cfg.Model = model
	}

	llmProvider, err := llm.NewProvider(ctx, cfg)
	if err != nil {
		http.Error(w, `{"error":"AI service unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	defer llmProvider.Close()

	// Build replan prompt
	daysToReplan := []types.DayPlan{}
	for _, idx := range rainyDays {
		if idx < len(itinerary) {
			daysToReplan = append(daysToReplan, itinerary[idx])
		}
	}
	daysJSON, _ := json.Marshal(daysToReplan)

	prompt := fmt.Sprintf(`You are an expert travel planner. The following days of a trip to %s have BAD WEATHER (rain/storms).
Replace outdoor activities with indoor alternatives while keeping the same time structure and budget.

Days to replan (JSON):
%s

Weather conditions:
%v

Requirements:
- Keep the same dayIndex, date, and time slots
- Replace outdoor activities (Nature, Attraction with outdoor venues) with indoor alternatives (museums, galleries, indoor markets, cooking classes, spa, etc.)
- Keep restaurants and indoor activities as-is
- Maintain similar budget levels
- Return ONLY a JSON array of the replanned days with the same structure

Return a JSON array of replanned day objects.`, destination, string(daysJSON), weatherInfo)

	jsonStr, err := llmProvider.GenerateJSON(ctx, prompt)
	if err != nil {
		log.Printf("Replan: Provider error: %v", err)
		http.Error(w, `{"error":"failed to generate alternatives"}`, http.StatusInternalServerError)
		return
	}

	jsonStr = strings.TrimSpace(jsonStr)
	if start := strings.Index(jsonStr, "["); start != -1 {
		if end := strings.LastIndex(jsonStr, "]"); end > start {
			jsonStr = jsonStr[start : end+1]
		}
	}

	var replannedDays []types.DayPlan
	if err := json.Unmarshal([]byte(jsonStr), &replannedDays); err != nil {
		log.Printf("Replan: Parse error: %v", err)
		http.Error(w, `{"error":"failed to parse replanned itinerary"}`, http.StatusInternalServerError)
		return
	}

	// Merge replanned days into original itinerary
	replannedMap := map[int]types.DayPlan{}
	for _, day := range replannedDays {
		replannedMap[day.DayIndex] = day
	}
	for i := range itinerary {
		if replacement, ok := replannedMap[itinerary[i].DayIndex]; ok {
			itinerary[i] = replacement
		}
	}

	// Save updated itinerary
	updatedJSON, _ := json.Marshal(itinerary)
	s.db.Exec(`UPDATE trips SET itinerary = $1, updated_at = NOW() WHERE request_id = $2`,
		string(updatedJSON), tripID)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message":    fmt.Sprintf("Replanned %d day(s) with indoor alternatives", len(rainyDays)),
		"rainy_days": rainyDays,
		"weather":    weatherInfo,
		"replanned":  true,
		"itinerary":  itinerary,
	})
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
				metrics.TripsCompletedTotal.Inc()
				log.Printf("Itinerary saved for %s", event.Data.RequestID)
				// The route worker holds no database, so the request carries the
				// places this trip already found; whatever is missing gets
				// geocoded. A failure here costs the trip its route, not its
				// itinerary, so it is logged rather than retried.
				s.requestRoute(ctx, event.Data.RequestID, event.Data.Destination, event.Data.Itinerary)
				msg.Ack()
			}
		}
	}
}

// requestRoute asks the route worker to order the trip's days, handing it the
// places already stored for the trip so it geocodes only what it has to.
// Routing is a nice-to-have on top of a finished itinerary: every failure here
// is logged and swallowed rather than propagated.
func (s *Server) requestRoute(ctx context.Context, requestID, destination string, itinerary []types.DayPlan) {
	if len(itinerary) == 0 {
		return
	}

	var placesJSON []byte
	// Places may still be in flight when the itinerary lands — that is fine, the
	// worker falls back to geocoding.
	if err := s.db.QueryRow(
		`SELECT COALESCE(places, '{}'::jsonb) FROM trips WHERE request_id = $1`, requestID,
	).Scan(&placesJSON); err != nil {
		log.Printf("Route: could not read places for %s: %v", requestID, err)
	}

	if err := route.PublishRequest(ctx, s.js, route.RequestData{
		RequestID:   requestID,
		Destination: destination,
		Itinerary:   itinerary,
		Places:      flattenPlaces(placesJSON),
	}); err != nil {
		log.Printf("Route: could not request a route for %s: %v", requestID, err)
	}
}

// flattenPlaces unpacks the stored `{hotels:[], restaurants:[], attractions:[]}`
// shape into one list. An older row may already hold a flat array, so both are
// accepted rather than failing the whole route.
func flattenPlaces(raw []byte) []geo.Place {
	if len(raw) == 0 {
		return nil
	}

	var flat []geo.Place
	if err := json.Unmarshal(raw, &flat); err == nil {
		return flat
	}

	var grouped map[string][]geo.Place
	if err := json.Unmarshal(raw, &grouped); err != nil {
		return nil
	}
	var out []geo.Place
	for _, group := range grouped {
		out = append(out, group...)
	}
	return out
}

// consumeRoutes persists computed routes. The route column is the only thing it
// touches: a trip is already 'completed' by the time its route is calculated.
func (s *Server) consumeRoutes(ctx context.Context) {
	var cons jetstream.Consumer
	for {
		stream, err := s.js.Stream(ctx, "TRIPS")
		if err == nil {
			cons, err = stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
				Durable:       "api-route-writer-v1",
				FilterSubject: route.SubjectResult,
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
			var envelope route.ResultEnvelope
			if err := json.Unmarshal(msg.Data(), &envelope); err != nil {
				msg.Ack()
				continue
			}

			routeBytes, _ := json.Marshal(envelope.Data.Route)
			if _, err := s.db.Exec(
				`UPDATE trips SET route = $1, updated_at = NOW() WHERE request_id = $2`,
				string(routeBytes), envelope.Data.RequestID,
			); err != nil {
				msg.Nak()
				continue
			}
			log.Printf("Route saved for %s", envelope.Data.RequestID)
			msg.Ack()
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

// =====================================================
// CONSUMER: FAILURES (status='failed')
// =====================================================

func (s *Server) consumeFailures(ctx context.Context) {
	js, _ := jetstream.New(s.nc)

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
		log.Printf("Failures consumer failed to get stream: %v", err)
		return
	}

	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       "api-failures-writer-v1",
		FilterSubject: "trips.failed",
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	if err != nil {
		log.Printf("Failed to create failures consumer: %v", err)
		return
	}

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
			var event types.TripFailedEvent
			if err := json.Unmarshal(msg.Data(), &event); err != nil {
				msg.Ack()
				continue
			}

			// Save to Postgres
			_, err := s.db.Exec(`UPDATE trips SET status = 'failed', error_reason = $1, updated_at = NOW() WHERE request_id = $2 AND status = 'pending'`,
				event.Data.Reason, event.Data.RequestID)

			if err != nil {
				log.Printf("Failed to save failure to DB: %v", err)
				msg.Nak()
			} else {
				metrics.TripsFailedTotal.Inc()
				log.Printf("Failure saved to DB for %s", event.Data.RequestID)
				msg.Ack()
			}
		}
	}
}
