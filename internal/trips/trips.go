// Package trips creates trips. Both the HTTP API and the MCP tools go through Store,
// so validation, key resolution and the "write the trip and its event atomically"
// rule live in one place.
package trips

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/sky-walkerx/trequila/internal/bus"
	"github.com/sky-walkerx/trequila/internal/events"
	"github.com/sky-walkerx/trequila/internal/metrics"
	"github.com/sky-walkerx/trequila/internal/outbox"
	"github.com/sky-walkerx/trequila/internal/providerkeys"
	"github.com/sky-walkerx/trequila/internal/types"
)

// ErrIdempotencyConflict means an Idempotency-Key was reused with a different request.
var ErrIdempotencyConflict = errors.New("trips: idempotency key was already used with a different request")

// ValidationError describes a request the caller can fix.
type ValidationError struct{ Message string }

func (e *ValidationError) Error() string { return e.Message }

// CreateParams is a request to plan a trip.
type CreateParams struct {
	UserID      string
	Source      string
	Destination string
	StartDate   string // YYYY-MM-DD
	EndDate     string // YYYY-MM-DD
	Days        int    // 0 = the inclusive span of the dates
	Budget      int
	Interests   []string
	Provider    string // empty = the user's default
	Model       string // empty = the provider's default

	// IdempotencyKey, when set, makes retries of the same request return the first
	// result instead of creating another trip.
	IdempotencyKey string
}

// Store creates trips.
type Store struct {
	DB    *sql.DB
	Keys  *providerkeys.Store
	Relay *outbox.Relay // optional; without it the periodic sweep publishes
}

// Create validates the request, resolves the caller's LLM key, and in one transaction
// stores the trip, its owner membership and the trips.requested event. It returns the
// trip's request ID and whether this call created it (false for an idempotent replay).
// providerkeys.ErrNoKey is returned, unwrapped-compatible, when the user has no key.
func (s *Store) Create(ctx context.Context, p CreateParams) (string, bool, error) {
	days, err := validate(p)
	if err != nil {
		return "", false, err
	}
	hash := requestHash(p)

	if p.IdempotencyKey != "" {
		if id, found, err := s.replay(ctx, s.DB, p.UserID, p.IdempotencyKey, hash); err != nil || found {
			return id, false, err
		}
	}

	cfg, err := s.Keys.Resolve(ctx, p.UserID, p.Provider)
	if err != nil {
		return "", false, err
	}
	provider, model := p.Provider, p.Model
	if provider == "" {
		provider = string(cfg.Kind)
	}
	if model == "" {
		model = cfg.Model
	}

	requestID := uuid.NewString()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()

	if p.IdempotencyKey != "" {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO idempotency_keys (user_id, key, request_hash, trip_id) VALUES ($1, $2, $3, $4)
			 ON CONFLICT DO NOTHING`, p.UserID, p.IdempotencyKey, hash, requestID)
		if err != nil {
			return "", false, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			// Another request with this key committed while we were preparing.
			tx.Rollback()
			id, _, err := s.replay(ctx, s.DB, p.UserID, p.IdempotencyKey, hash)
			return id, false, err
		}
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO trips (request_id, user_id, source, destination, start_date, end_date, budget, status, provider, model)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, 'pending', $8, $9)`,
		requestID, p.UserID, p.Source, p.Destination, p.StartDate, p.EndDate, p.Budget, provider, model); err != nil {
		return "", false, fmt.Errorf("trips: insert: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO trip_members (trip_id, user_id, role) VALUES ($1, $2, 'owner')`, requestID, p.UserID); err != nil {
		return "", false, fmt.Errorf("trips: add owner: %w", err)
	}

	interests := p.Interests
	if interests == nil {
		interests = []string{}
	}
	ev, err := bus.NewEvent(events.TripRequested, events.ID(events.TripRequested, requestID), types.TripRequest{
		RequestID:   requestID,
		UserID:      p.UserID,
		Source:      p.Source,
		Destination: p.Destination,
		StartDate:   p.StartDate,
		EndDate:     p.EndDate,
		Days:        days,
		Budget:      p.Budget,
		Interests:   interests,
		Provider:    provider,
		Model:       model,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return "", false, err
	}
	if err := outbox.Add(ctx, tx, ev); err != nil {
		return "", false, err
	}
	if err := tx.Commit(); err != nil {
		return "", false, fmt.Errorf("trips: commit: %w", err)
	}

	metrics.TripsCreatedTotal.Inc()
	if s.Relay != nil {
		s.Relay.Kick(ctx)
	}
	return requestID, true, nil
}

type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// replay looks up an earlier request with the same key.
func (s *Store) replay(ctx context.Context, q queryer, userID, key, hash string) (id string, found bool, err error) {
	var storedHash string
	err = q.QueryRowContext(ctx,
		`SELECT request_hash, trip_id FROM idempotency_keys WHERE user_id = $1 AND key = $2`, userID, key).Scan(&storedHash, &id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if storedHash != hash {
		return "", true, ErrIdempotencyConflict
	}
	return id, true, nil
}

// Limits on what a request may carry. They keep a single request from producing an
// event the broker refuses or a prompt no model will accept.
const (
	maxTextLen       = 200
	maxInterests     = 20
	maxInterestLen   = 60
	maxProviderField = 100
	maxTripDays      = 30
)

func validate(p CreateParams) (days int, err error) {
	if p.UserID == "" {
		return 0, &ValidationError{"user is required"}
	}
	if p.Source == "" || p.Destination == "" || p.StartDate == "" || p.EndDate == "" {
		return 0, &ValidationError{"source, destination, start_date and end_date are required"}
	}
	for _, v := range []string{p.Source, p.Destination} {
		if len(v) > maxTextLen {
			return 0, &ValidationError{"source and destination must be at most 200 characters"}
		}
	}
	if len(p.Provider) > maxProviderField || len(p.Model) > maxProviderField {
		return 0, &ValidationError{"provider and model must be at most 100 characters"}
	}
	if len(p.Interests) > maxInterests {
		return 0, &ValidationError{"at most 20 interests are allowed"}
	}
	for _, in := range p.Interests {
		if len(in) > maxInterestLen {
			return 0, &ValidationError{"each interest must be at most 60 characters"}
		}
	}
	if p.Days < 0 || p.Days > maxTripDays || p.Budget < 0 {
		return 0, &ValidationError{"days must be between 0 and 30 and budget must not be negative"}
	}
	start, err := time.Parse("2006-01-02", p.StartDate)
	if err != nil {
		return 0, &ValidationError{"invalid start_date format, expected YYYY-MM-DD"}
	}
	end, err := time.Parse("2006-01-02", p.EndDate)
	if err != nil {
		return 0, &ValidationError{"invalid end_date format, expected YYYY-MM-DD"}
	}
	if end.Before(start) {
		return 0, &ValidationError{"end_date must not be before start_date"}
	}
	days = p.Days
	if days == 0 {
		days = int(end.Sub(start).Hours()/24) + 1
	}
	if days > maxTripDays {
		return 0, &ValidationError{"trips are limited to 30 days"}
	}
	return days, nil
}

func requestHash(p CreateParams) string {
	p.IdempotencyKey = ""
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// PurgeIdempotencyKeys deletes keys older than olderThan, which ends their replay
// window, and returns how many it removed.
func (s *Store) PurgeIdempotencyKeys(ctx context.Context, olderThan time.Duration) (int64, error) {
	res, err := s.DB.ExecContext(ctx,
		`DELETE FROM idempotency_keys WHERE created_at < now() - make_interval(secs => $1)`, olderThan.Seconds())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
