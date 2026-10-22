package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/sky-walkerx/trequila/internal/providerkeys"
	"github.com/sky-walkerx/trequila/internal/trips"
)

const maxIdempotencyKeyLen = 255

func (s *Service) handleCreateUserTrip(w http.ResponseWriter, r *http.Request, userID string) {
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
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	key := r.Header.Get("Idempotency-Key")
	if len(key) > maxIdempotencyKeyLen {
		writeError(w, http.StatusBadRequest, "Idempotency-Key is too long")
		return
	}

	id, created, err := s.trips.Create(r.Context(), trips.CreateParams{
		UserID:         userID,
		Source:         req.Source,
		Destination:    req.Destination,
		StartDate:      req.StartDate,
		EndDate:        req.EndDate,
		Days:           req.Days,
		Budget:         req.Budget,
		Interests:      req.Interests,
		Provider:       req.Provider,
		Model:          req.Model,
		IdempotencyKey: key,
	})
	if err != nil {
		var verr *trips.ValidationError
		switch {
		case errors.As(err, &verr):
			writeError(w, http.StatusBadRequest, verr.Message)
		case errors.Is(err, providerkeys.ErrNoKey):
			writeError(w, http.StatusBadRequest, "add an API key in Settings")
		case errors.Is(err, trips.ErrIdempotencyConflict):
			writeError(w, http.StatusUnprocessableEntity, "Idempotency-Key was already used with a different request")
		default:
			s.log.Error("create trip failed", "user_id", userID, "error", err)
			writeError(w, http.StatusInternalServerError, "database error")
		}
		return
	}

	if !created {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"request_id": id,
		"status":     "pending",
		"message":    "Trip request accepted",
	})
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
