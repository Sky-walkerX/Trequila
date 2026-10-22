package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/sky-walkerx/trequila/internal/auth"
)

func (s *Service) handleGoogleLogin(w http.ResponseWriter, r *http.Request) {
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

func (s *Service) handleGoogleCallback(w http.ResponseWriter, r *http.Request) {
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

func (s *Service) handleRegister(w http.ResponseWriter, r *http.Request) {
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

func (s *Service) handleLogin(w http.ResponseWriter, r *http.Request) {
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

func (s *Service) handleGetCurrentUser(w http.ResponseWriter, r *http.Request) {
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
