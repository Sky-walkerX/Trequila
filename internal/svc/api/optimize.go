package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/sky-walkerx/trequila/internal/auth"
	"github.com/sky-walkerx/trequila/internal/types"
)

// handleOptimizeTripRequest recomputes a trip's route. Ordering a long trip means
// dozens of rate-limited geocoder lookups, far past a typical request timeout, so
// this only queues the work; the client watches the stream (or polls GET
// /api/trips/{id}) for the new route.
func (s *Service) handleOptimizeTripRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	userID := auth.GetUserIDFromContext(r.Context())
	if userID == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	tripID := strings.TrimPrefix(r.URL.Path, "/api/trips/optimize/")
	if tripID == "" {
		writeError(w, http.StatusBadRequest, "trip id required")
		return
	}

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	defer tx.Rollback()

	var destination string
	var itineraryJSON []byte
	err = tx.QueryRowContext(r.Context(),
		`SELECT destination, itinerary FROM trips WHERE request_id = $1 AND user_id = $2`, tripID, userID,
	).Scan(&destination, &itineraryJSON)
	if err == sql.ErrNoRows {
		writeError(w, http.StatusNotFound, "trip not found")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}

	var itinerary []types.DayPlan
	if err := json.Unmarshal(itineraryJSON, &itinerary); err != nil || len(itinerary) == 0 {
		writeError(w, http.StatusConflict, "trip has no itinerary to optimize")
		return
	}

	if err := s.addRouteRequest(r.Context(), tx, tripID, destination, itinerary, "optimize-"+uuid.NewString()); err != nil {
		s.log.Error("queue route request failed", "trip_id", tripID, "error", err)
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}
	if s.relay != nil {
		s.relay.Kick(r.Context())
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"status": "optimizing", "request_id": tripID})
}
