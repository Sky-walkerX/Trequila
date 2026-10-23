// Package api is the public HTTP service: authentication, trips, provider keys and
// the live progress stream. It also owns the trips table, so it consumes the other
// services' result events and writes them to Postgres together with the trip events
// the stream replays.
package api

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sky-walkerx/trequila/internal/auth"
	"github.com/sky-walkerx/trequila/internal/bus"
	"github.com/sky-walkerx/trequila/internal/inbox"
	"github.com/sky-walkerx/trequila/internal/outbox"
	"github.com/sky-walkerx/trequila/internal/providerkeys"
	"github.com/sky-walkerx/trequila/internal/tripevents"
	"github.com/sky-walkerx/trequila/internal/trips"
)

// Deps are the collaborators the service needs.
type Deps struct {
	DB          *sql.DB
	Auth        *auth.AuthService
	Keys        *providerkeys.Store
	Trips       *trips.Store
	Hub         *tripevents.Hub
	Relay       *outbox.Relay
	Pinger      bus.Pinger // optional; reported by /health
	FrontendURL string
	Log         *slog.Logger

	// MetricsToken protects /metrics. When empty the endpoint is not served at all:
	// request counts and route names are not for the public internet.
	MetricsToken string
}

// Service implements the API.
type Service struct {
	db           *sql.DB
	auth         *auth.AuthService
	keys         *providerkeys.Store
	trips        *trips.Store
	hub          *tripevents.Hub
	relay        *outbox.Relay
	pinger       bus.Pinger
	frontendURL  string
	log          *slog.Logger
	metricsToken string

	closing     chan struct{} // closed by BeginShutdown so open streams let go
	closingOnce sync.Once

	// Stream timings; fields so tests can shorten them.
	streamHeartbeat   time.Duration
	streamHardTimeout time.Duration
	streamPlacesGrace time.Duration
}

// Stream timings. The heartbeat must stay well under the ~55s idle window enforced by
// Heroku's router (error H15) and most reverse proxies.
const (
	defaultStreamHeartbeat   = 20 * time.Second
	defaultStreamHardTimeout = 5 * time.Minute
	// defaultStreamPlacesGrace is how long to keep streaming after the itinerary lands,
	// waiting for places, before declaring the trip complete with partial data.
	defaultStreamPlacesGrace = 30 * time.Second
)

// New creates the service.
func New(d Deps) *Service {
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		db: d.DB, auth: d.Auth, keys: d.Keys, trips: d.Trips, hub: d.Hub, relay: d.Relay,
		pinger: d.Pinger, frontendURL: d.FrontendURL, log: log, metricsToken: d.MetricsToken,
		closing:           make(chan struct{}),
		streamHeartbeat:   defaultStreamHeartbeat,
		streamHardTimeout: defaultStreamHardTimeout,
		streamPlacesGrace: defaultStreamPlacesGrace,
	}
}

// BeginShutdown tells long-lived responses (the SSE streams) to finish, so that
// http.Server.Shutdown does not wait out its whole deadline for them. Clients reconnect
// to another instance and resume from Last-Event-ID.
func (s *Service) BeginShutdown() { s.closingOnce.Do(func() { close(s.closing) }) }

// Routes registers the service's HTTP routes.
func (s *Service) Routes(mux *http.ServeMux) {
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/ping", s.handlePing)
	mux.HandleFunc("/health", s.handleHealth)
	mux.Handle("/metrics", s.metricsHandler())

	mux.HandleFunc("/auth/register", s.handleRegister)
	mux.HandleFunc("/auth/login", s.handleLogin)
	mux.HandleFunc("/auth/google/login", s.handleGoogleLogin)
	mux.HandleFunc("/auth/google/callback", s.handleGoogleCallback)

	protected := func(h http.Handler) http.Handler { return s.auth.AuthMiddleware(h) }
	mux.Handle("/api/me", protected(http.HandlerFunc(s.handleGetCurrentUser)))
	mux.Handle("/api/trips", protected(http.HandlerFunc(s.instrumentHandler("/api/trips", s.handleTrips))))
	mux.Handle("/api/trips/", protected(http.HandlerFunc(s.instrumentHandler("/api/trips/{id}", s.handleTripByID))))
	mux.Handle("/api/trips/replan/", protected(http.HandlerFunc(s.instrumentHandler("/api/trips/replan", s.handleWeatherReplan))))
	// More specific than "/api/trips/", so ServeMux routes it here, not to handleTripByID.
	mux.Handle("/api/trips/optimize/", protected(http.HandlerFunc(s.instrumentHandler("/api/trips/optimize", s.handleOptimizeTripRequest))))

	mux.Handle("/api/keys", protected(http.HandlerFunc(s.handleKeys)))
	mux.Handle("/api/keys/test", protected(http.HandlerFunc(s.handleKeysTest)))
	mux.Handle("/api/keys/", protected(http.HandlerFunc(s.handleKeyByProvider)))
}

// Run starts the service's background jobs and blocks until ctx is cancelled:
// failing trips that never got an itinerary, and forgetting old processed-event
// markers.
func (s *Service) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); s.reapLoop(ctx) }()
	go func() { defer wg.Done(); s.purgeLoop(ctx) }()
	wg.Wait()
}

// processedEventsRetention must stay well above the broker's redelivery horizon.
const processedEventsRetention = 14 * 24 * time.Hour

func (s *Service) purgeLoop(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if n, err := inbox.Purge(ctx, s.db, processedEventsRetention); err != nil {
			if ctx.Err() == nil {
				s.log.Warn("purging processed events failed", "error", err)
			}
		} else if n > 0 {
			s.log.Info("purged processed events", "rows", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// metricsHandler serves Prometheus metrics to a caller presenting the configured bearer
// token, and 404s when no token is configured.
func (s *Service) metricsHandler() http.Handler {
	if s.metricsToken == "" {
		return http.NotFoundHandler()
	}
	want := []byte(s.metricsToken)
	prom := promhttp.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		prom.ServeHTTP(w, r)
	})
}
