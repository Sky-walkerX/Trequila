package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/sky-walkerx/trequila/internal/ai"
	"github.com/sky-walkerx/trequila/internal/auth"
	"github.com/sky-walkerx/trequila/internal/llm"
	"github.com/sky-walkerx/trequila/internal/providerkeys"
)

func (s *Server) handleKeys(w http.ResponseWriter, r *http.Request) {
	userID := auth.GetUserIDFromContext(r.Context())
	if userID == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	switch r.Method {
	case http.MethodGet:
		keys, err := s.keys.List(r.Context(), userID)
		if err != nil {
			http.Error(w, "Failed to list keys: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if keys == nil {
			keys = []providerkeys.ProviderKey{} // Return [] instead of null
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(keys)

	case http.MethodPost:
		var req struct {
			Provider     string `json:"provider"`
			APIKey       string `json:"api_key"`
			BaseURL      string `json:"base_url"`
			DefaultModel string `json:"default_model"`
			MakeDefault  bool   `json:"make_default"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		if req.Provider == "" || (req.APIKey == "" && req.Provider != "openai_compatible") || req.DefaultModel == "" {
			http.Error(w, "Missing required fields", http.StatusBadRequest)
			return
		}

		if err := s.keys.Upsert(r.Context(), userID, req.Provider, req.APIKey, req.BaseURL, req.DefaultModel, req.MakeDefault); err != nil {
			http.Error(w, "Failed to save key: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleKeyByProvider(w http.ResponseWriter, r *http.Request) {
	userID := auth.GetUserIDFromContext(r.Context())
	if userID == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Extract provider from path /api/keys/{provider}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 3 {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}
	provider := parts[2]

	if r.Method == http.MethodDelete {
		if err := s.keys.Delete(r.Context(), userID, provider); err != nil {
			http.Error(w, "Failed to delete key: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

// handleKeysTest validates a provider key with one cheap live call before the
// user saves it. The key may be supplied in the body (pre-save check) or omitted
// to re-test the key already stored for that provider.
func (s *Server) handleKeysTest(w http.ResponseWriter, r *http.Request) {
	userID := auth.GetUserIDFromContext(r.Context())
	if userID == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Provider string `json:"provider"`
		APIKey   string `json:"api_key"`
		BaseURL  string `json:"base_url"`
		Model    string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	var cfg ai.ProviderConfig
	if req.APIKey != "" || req.Provider == string(ai.ProviderOpenAICompatible) {
		if req.Provider == "" {
			http.Error(w, "Missing provider", http.StatusBadRequest)
			return
		}
		cfg = ai.ProviderConfig{
			Kind:    ai.ProviderKind(req.Provider),
			APIKey:  req.APIKey,
			BaseURL: req.BaseURL,
			Model:   req.Model,
		}
	} else {
		// No key in the body: test what is already stored.
		stored, err := s.keys.Resolve(r.Context(), userID, req.Provider)
		if err != nil {
			if errors.Is(err, providerkeys.ErrNoKey) {
				writeKeyTestResult(w, http.StatusBadRequest, false, "no key configured for this provider")
			} else {
				writeKeyTestResult(w, http.StatusInternalServerError, false, "failed to resolve stored key")
			}
			return
		}
		cfg = stored
		if req.Model != "" {
			cfg.Model = req.Model
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	provider, err := llm.NewProvider(ctx, cfg)
	if err != nil {
		writeKeyTestResult(w, http.StatusBadRequest, false, err.Error())
		return
	}
	defer provider.Close()

	if _, err := provider.Chat(ctx, "Reply with the single word: ok.", nil, "ping"); err != nil {
		status := http.StatusBadGateway
		if ai.IsAuthError(err) {
			status = http.StatusUnauthorized
		}
		writeKeyTestResult(w, status, false, err.Error())
		return
	}

	writeKeyTestResult(w, http.StatusOK, true, "")
}

func writeKeyTestResult(w http.ResponseWriter, status int, ok bool, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := map[string]interface{}{"ok": ok}
	if reason != "" {
		body["error"] = reason
	}
	json.NewEncoder(w).Encode(body)
}
