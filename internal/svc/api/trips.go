package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sky-walkerx/trequila/internal/auth"
	"github.com/sky-walkerx/trequila/internal/metrics"
)

func (s *Service) instrumentHandler(path string, handler func(http.ResponseWriter, *http.Request)) func(http.ResponseWriter, *http.Request) {
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

func (s *Service) handleTrips(w http.ResponseWriter, r *http.Request) {
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

func (s *Service) handleTripByID(w http.ResponseWriter, r *http.Request) {
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

func (s *Service) handleListUserTrips(w http.ResponseWriter, r *http.Request, userID string) {
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
	if err := s.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM trips WHERE user_id = $1`, userID).Scan(&total); err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}

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

func (s *Service) handleGetUserTrip(w http.ResponseWriter, r *http.Request, userID, tripID string) {
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

func (s *Service) handleDeleteUserTrip(w http.ResponseWriter, r *http.Request, userID, tripID string) {
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
