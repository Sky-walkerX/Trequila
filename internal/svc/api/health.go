package api

import (
	"encoding/json"
	"net/http"
	"time"
)

func (s *Service) handleRoot(w http.ResponseWriter, r *http.Request) {
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
func (s *Service) handlePing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}

// handleHealth checks DB and NATS connectivity (may be slow if DB is cold)
func (s *Service) handleHealth(w http.ResponseWriter, r *http.Request) {
	health := map[string]interface{}{
		"status":    "healthy",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"services":  map[string]string{},
	}

	services := health["services"].(map[string]string)

	if err := s.db.PingContext(r.Context()); err != nil {
		services["database"] = "unhealthy"
		health["status"] = "degraded"
	} else {
		services["database"] = "healthy"
	}

	if s.pinger != nil {
		if err := s.pinger.Ping(r.Context()); err != nil {
			services["nats"] = "unhealthy"
			health["status"] = "degraded"
		} else {
			services["nats"] = "healthy"
		}
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
