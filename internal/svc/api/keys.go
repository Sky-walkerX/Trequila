package api

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
	"github.com/sky-walkerx/trequila/internal/netguard"
	"github.com/sky-walkerx/trequila/internal/providerkeys"
)

func (s *Service) handleKeys(w http.ResponseWriter, r *http.Request) {
	userID := auth.GetUserIDFromContext(r.Context())
	if userID == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	switch r.Method {
	case http.MethodGet:
		keys, err := s.keys.List(r.Context(), userID)
		if err != nil {
			s.log.Error("list keys failed", "user_id", userID, "error", err)
			http.Error(w, "Failed to list keys", http.StatusInternalServerError)
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
			if errors.Is(err, providerkeys.ErrInvalidBaseURL) {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			s.log.Error("save key failed", "user_id", userID, "provider", req.Provider, "error", err)
			http.Error(w, "Failed to save key", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Service) handleKeyByProvider(w http.ResponseWriter, r *http.Request) {
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
			s.log.Error("delete key failed", "user_id", userID, "provider", provider, "error", err)
			http.Error(w, "Failed to delete key", http.StatusInternalServerError)
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
func (s *Service) handleKeysTest(w http.ResponseWriter, r *http.Request) {
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
		if req.BaseURL != "" {
			if _, err := netguard.ValidateBaseURL(req.BaseURL, netguard.AllowPrivateFromEnv()); err != nil {
				writeKeyTestResult(w, http.StatusBadRequest, false, err.Error())
				return
			}
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
		s.log.Info("key test: provider could not be built", "user_id", userID, "provider", cfg.Kind, "error", err)
		writeKeyTestResult(w, http.StatusBadRequest, false, "unsupported provider or model")
		return
	}
	defer provider.Close()

	if _, err := provider.Chat(ctx, "Reply with the single word: ok.", nil, "ping"); err != nil {
		// Whatever the upstream said stays in the server log. Echoing it would let a
		// caller read responses from arbitrary hosts through this endpoint.
		s.log.Info("key test failed", "user_id", userID, "provider", cfg.Kind, "error", err)
		switch {
		case errors.Is(err, netguard.ErrBlockedAddress):
			writeKeyTestResult(w, http.StatusBadRequest, false, "that address is not allowed")
		case ai.IsAuthError(err):
			writeKeyTestResult(w, http.StatusUnauthorized, false, "the provider rejected the credentials")
		default:
			writeKeyTestResult(w, http.StatusBadGateway, false, "could not get a response from the provider")
		}
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
