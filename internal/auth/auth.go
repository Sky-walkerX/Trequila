package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

var (
	ErrInvalidToken     = errors.New("invalid token")
	ErrExpiredToken     = errors.New("token expired")
	ErrMissingToken     = errors.New("missing authorization token")
	ErrInvalidClaims    = errors.New("invalid token claims")
	ErrUserExists       = errors.New("user already exists")
	ErrUserNotFound     = errors.New("user not found")
	ErrInvalidPassword  = errors.New("invalid password")
	ErrInvalidEmail     = errors.New("invalid email")
	ErrPasswordRequired = errors.New("password required for local accounts")
	ErrWeakPassword     = errors.New("password must be at least 8 characters")
	ErrOAuthAccount     = errors.New("this account uses OAuth login, please use Google sign-in")
)

// Claims represents JWT claims
type Claims struct {
	UserID   string `json:"user_id"`
	Email    string `json:"email"`
	Name     string `json:"name"`
	Provider string `json:"provider"` // "local", "google", etc.
	jwt.RegisteredClaims
}

// User represents a user in the database
type User struct {
	ID           string
	Email        string
	Name         string
	PasswordHash sql.NullString
	Provider     string
	ProviderID   sql.NullString
	AvatarURL    sql.NullString
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// AuthService handles authentication
type AuthService struct {
	db           *sql.DB
	jwtSecret    []byte
	jwtExpiry    time.Duration
	googleConfig *oauth2.Config
}

// NewAuthService creates a new auth service
func NewAuthService(db *sql.DB) *AuthService {
	jwtSecret := os.Getenv("JWT_SECRET")
	if len(jwtSecret) < 32 {
		panic("JWT_SECRET must be set and at least 32 bytes long")
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
		db:           db,
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

	// Fallback to cookie
	if cookie, err := r.Cookie("auth_token"); err == nil {
		return cookie.Value
	}

	return ""
}

type contextKey string

const (
	claimsKey contextKey = "claims"
	userIDKey contextKey = "user_id"
)

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
		ctx := context.WithValue(r.Context(), claimsKey, claims)
		ctx = context.WithValue(ctx, userIDKey, claims.UserID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// GetUserIDFromContext retrieves user ID from context
func GetUserIDFromContext(ctx context.Context) string {
	if userID, ok := ctx.Value(userIDKey).(string); ok {
		return userID
	}
	return ""
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

	// Get user info from Google
	client := a.googleConfig.Client(ctx, token)
	resp, err := client.Get("https://www.googleapis.com/oauth2/v2/userinfo")
	if err != nil {
		return nil, "", fmt.Errorf("failed to get user info: %w", err)
	}
	defer resp.Body.Close()

	var googleUser GoogleUser
	if err := json.NewDecoder(resp.Body).Decode(&googleUser); err != nil {
		return nil, "", fmt.Errorf("failed to decode user info: %w", err)
	}

	// Find or create user in database
	user, err := a.FindOrCreateOAuthUser(ctx, googleUser.Email, googleUser.Name, "google", googleUser.ID, googleUser.Picture)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create user: %w", err)
	}

	// Generate JWT for the user
	jwtToken, err := a.GenerateToken(user.ID, user.Email, user.Name, "google")
	if err != nil {
		return nil, "", fmt.Errorf("failed to generate token: %w", err)
	}

	return &googleUser, jwtToken, nil
}

// ===== User Management =====

// RegisterUser creates a new user with email and password
func (a *AuthService) RegisterUser(ctx context.Context, email, password, name string) (*User, string, error) {
	if email == "" || !strings.Contains(email, "@") {
		return nil, "", ErrInvalidEmail
	}
	if password == "" {
		return nil, "", ErrPasswordRequired
	}
	if len(password) < 8 {
		return nil, "", ErrWeakPassword
	}

	// Check if user already exists
	var existingID string
	err := a.db.QueryRowContext(ctx, `SELECT id FROM users WHERE email = $1`, email).Scan(&existingID)
	if err == nil {
		return nil, "", ErrUserExists
	} else if err != sql.ErrNoRows {
		return nil, "", fmt.Errorf("database error: %w", err)
	}

	// Hash password
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, "", fmt.Errorf("failed to hash password: %w", err)
	}

	// Create user
	userID := uuid.NewString()
	_, err = a.db.ExecContext(ctx, `
		INSERT INTO users (id, email, name, password_hash, provider)
		VALUES ($1, $2, $3, $4, 'local')`,
		userID, email, name, string(hashedPassword),
	)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create user: %w", err)
	}

	user := &User{
		ID:       userID,
		Email:    email,
		Name:     name,
		Provider: "local",
	}

	// Generate token
	token, err := a.GenerateToken(userID, email, name, "local")
	if err != nil {
		return nil, "", fmt.Errorf("failed to generate token: %w", err)
	}

	return user, token, nil
}

// LoginUser authenticates a user with email and password
func (a *AuthService) LoginUser(ctx context.Context, email, password string) (*User, string, error) {
	if email == "" {
		return nil, "", ErrInvalidEmail
	}
	if password == "" {
		return nil, "", ErrPasswordRequired
	}

	var user User
	err := a.db.QueryRowContext(ctx, `
		SELECT id, email, name, password_hash, provider, provider_id, avatar_url, created_at, updated_at
		FROM users WHERE email = $1`,
		email,
	).Scan(&user.ID, &user.Email, &user.Name, &user.PasswordHash, &user.Provider,
		&user.ProviderID, &user.AvatarURL, &user.CreatedAt, &user.UpdatedAt)

	if err == sql.ErrNoRows {
		return nil, "", ErrUserNotFound
	} else if err != nil {
		return nil, "", fmt.Errorf("database error: %w", err)
	}

	// Check if user has a password (local account)
	if !user.PasswordHash.Valid || user.PasswordHash.String == "" {
		return nil, "", ErrOAuthAccount
	}

	// Verify password
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash.String), []byte(password)); err != nil {
		return nil, "", ErrInvalidPassword
	}

	// Generate token
	token, err := a.GenerateToken(user.ID, user.Email, user.Name, user.Provider)
	if err != nil {
		return nil, "", fmt.Errorf("failed to generate token: %w", err)
	}

	return &user, token, nil
}

// FindOrCreateOAuthUser finds an existing OAuth user or creates a new one
func (a *AuthService) FindOrCreateOAuthUser(ctx context.Context, email, name, provider, providerID, avatarURL string) (*User, error) {
	var user User

	// Try to find existing user by provider + provider_id
	err := a.db.QueryRowContext(ctx, `
		SELECT id, email, name, password_hash, provider, provider_id, avatar_url, created_at, updated_at
		FROM users WHERE provider = $1 AND provider_id = $2`,
		provider, providerID,
	).Scan(&user.ID, &user.Email, &user.Name, &user.PasswordHash, &user.Provider,
		&user.ProviderID, &user.AvatarURL, &user.CreatedAt, &user.UpdatedAt)

	if err == nil {
		// User exists, update their info
		_, _ = a.db.ExecContext(ctx, `
			UPDATE users SET name = $1, avatar_url = $2, updated_at = NOW() WHERE id = $3`,
			name, avatarURL, user.ID,
		)
		user.Name = name
		return &user, nil
	}

	if err != sql.ErrNoRows {
		return nil, fmt.Errorf("database error: %w", err)
	}

	// Check if email exists with different provider
	err = a.db.QueryRowContext(ctx, `SELECT id FROM users WHERE email = $1`, email).Scan(&user.ID)
	if err == nil {
		// Email exists - link this OAuth to existing account
		_, err = a.db.ExecContext(ctx, `
			UPDATE users SET provider = $1, provider_id = $2, avatar_url = $3, updated_at = NOW() WHERE email = $4`,
			provider, providerID, avatarURL, email,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to link account: %w", err)
		}
		// Fetch updated user
		_ = a.db.QueryRowContext(ctx, `
			SELECT id, email, name, password_hash, provider, provider_id, avatar_url, created_at, updated_at
			FROM users WHERE email = $1`,
			email,
		).Scan(&user.ID, &user.Email, &user.Name, &user.PasswordHash, &user.Provider,
			&user.ProviderID, &user.AvatarURL, &user.CreatedAt, &user.UpdatedAt)
		return &user, nil
	}

	// Create new user
	userID := uuid.NewString()
	_, err = a.db.ExecContext(ctx, `
		INSERT INTO users (id, email, name, provider, provider_id, avatar_url)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		userID, email, name, provider, providerID, avatarURL,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create user: %w", err)
	}

	return &User{
		ID:       userID,
		Email:    email,
		Name:     name,
		Provider: provider,
	}, nil
}

// GetUserByID retrieves a user by ID
func (a *AuthService) GetUserByID(ctx context.Context, userID string) (*User, error) {
	var user User
	err := a.db.QueryRowContext(ctx, `
		SELECT id, email, name, password_hash, provider, provider_id, avatar_url, created_at, updated_at
		FROM users WHERE id = $1`,
		userID,
	).Scan(&user.ID, &user.Email, &user.Name, &user.PasswordHash, &user.Provider,
		&user.ProviderID, &user.AvatarURL, &user.CreatedAt, &user.UpdatedAt)

	if err == sql.ErrNoRows {
		return nil, ErrUserNotFound
	} else if err != nil {
		return nil, fmt.Errorf("database error: %w", err)
	}

	return &user, nil
}
