package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

var (
	ErrInvalidToken  = errors.New("invalid token")
	ErrExpiredToken  = errors.New("token expired")
	ErrMissingToken  = errors.New("missing authorization token")
	ErrInvalidClaims = errors.New("invalid token claims")
)

// Claims represents JWT claims
type Claims struct {
	UserID   string `json:"user_id"`
	Email    string `json:"email"`
	Name     string `json:"name"`
	Provider string `json:"provider"` // "local", "google", etc.
	jwt.RegisteredClaims
}

// AuthService handles authentication
type AuthService struct {
	jwtSecret    []byte
	jwtExpiry    time.Duration
	googleConfig *oauth2.Config
}

// NewAuthService creates a new auth service
func NewAuthService() *AuthService {
	jwtSecret := os.Getenv("JWT_SECRET")
	if jwtSecret == "" {
		jwtSecret = "your-super-secret-key-change-in-production" // Default for dev
	}

	var googleConfig *oauth2.Config
	googleClientID := os.Getenv("GOOGLE_CLIENT_ID")
	googleClientSecret := os.Getenv("GOOGLE_CLIENT_SECRET")
	googleRedirectURL := os.Getenv("GOOGLE_REDIRECT_URL")

	if googleClientID != "" && googleClientSecret != "" {
		googleConfig = &oauth2.Config{
			ClientID:     googleClientID,
			ClientSecret: googleClientSecret,
			RedirectURL:  googleRedirectURL,
			Scopes:       []string{"email", "profile"},
			Endpoint:     google.Endpoint,
		}
	}

	return &AuthService{
		jwtSecret:    []byte(jwtSecret),
		jwtExpiry:    24 * time.Hour, // Token valid for 24 hours
		googleConfig: googleConfig,
	}
}

// GenerateToken creates a new JWT token for a user
func (a *AuthService) GenerateToken(userID, email, name, provider string) (string, error) {
	claims := &Claims{
		UserID:   userID,
		Email:    email,
		Name:     name,
		Provider: provider,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(a.jwtExpiry)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    "trequila",
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(a.jwtSecret)
}

// ValidateToken validates a JWT token and returns the claims
func (a *AuthService) ValidateToken(tokenString string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return a.jwtSecret, nil
	})

	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrExpiredToken
		}
		return nil, ErrInvalidToken
	}

	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, ErrInvalidClaims
	}

	return claims, nil
}

// ExtractToken extracts token from Authorization header
func ExtractToken(r *http.Request) string {
	bearerToken := r.Header.Get("Authorization")
	if len(bearerToken) > 7 && strings.ToUpper(bearerToken[0:7]) == "BEARER " {
		return bearerToken[7:]
	}
	return ""
}

// AuthMiddleware is the JWT authentication middleware
func (a *AuthService) AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenString := ExtractToken(r)
		if tokenString == "" {
			http.Error(w, `{"error":"missing authorization token"}`, http.StatusUnauthorized)
			return
		}

		claims, err := a.ValidateToken(tokenString)
		if err != nil {
			status := http.StatusUnauthorized
			message := err.Error()
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, message), status)
			return
		}

		// Add claims to context
		ctx := context.WithValue(r.Context(), "claims", claims)
		ctx = context.WithValue(ctx, "user_id", claims.UserID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// OptionalAuthMiddleware extracts user info if token present, but doesn't require it
func (a *AuthService) OptionalAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenString := ExtractToken(r)
		if tokenString != "" {
			claims, err := a.ValidateToken(tokenString)
			if err == nil {
				ctx := context.WithValue(r.Context(), "claims", claims)
				ctx = context.WithValue(ctx, "user_id", claims.UserID)
				r = r.WithContext(ctx)
			}
		}
		next.ServeHTTP(w, r)
	})
}

// GetUserIDFromContext retrieves user ID from context
func GetUserIDFromContext(ctx context.Context) string {
	if userID, ok := ctx.Value("user_id").(string); ok {
		return userID
	}
	return ""
}

// GetClaimsFromContext retrieves claims from context
func GetClaimsFromContext(ctx context.Context) *Claims {
	if claims, ok := ctx.Value("claims").(*Claims); ok {
		return claims
	}
	return nil
}

// ===== OAuth Handlers =====

// GoogleLoginURL returns the Google OAuth login URL
func (a *AuthService) GoogleLoginURL(state string) string {
	if a.googleConfig == nil {
		return ""
	}
	return a.googleConfig.AuthCodeURL(state, oauth2.AccessTypeOffline)
}

// GoogleUser represents user info from Google
type GoogleUser struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	VerifiedEmail bool   `json:"verified_email"`
	Name          string `json:"name"`
	Picture       string `json:"picture"`
}

// HandleGoogleCallback exchanges code for token and gets user info
func (a *AuthService) HandleGoogleCallback(ctx context.Context, code string) (*GoogleUser, string, error) {
	if a.googleConfig == nil {
		return nil, "", errors.New("google oauth not configured")
	}

	// Exchange code for token
	token, err := a.googleConfig.Exchange(ctx, code)
	if err != nil {
		return nil, "", fmt.Errorf("code exchange failed: %w", err)
	}

	// Get user info
	client := a.googleConfig.Client(ctx, token)
	resp, err := client.Get("https://www.googleapis.com/oauth2/v2/userinfo")
	if err != nil {
		return nil, "", fmt.Errorf("failed to get user info: %w", err)
	}
	defer resp.Body.Close()

	var user GoogleUser
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return nil, "", fmt.Errorf("failed to decode user info: %w", err)
	}

	// Generate JWT for the user
	jwtToken, err := a.GenerateToken(user.ID, user.Email, user.Name, "google")
	if err != nil {
		return nil, "", fmt.Errorf("failed to generate token: %w", err)
	}

	return &user, jwtToken, nil
}
