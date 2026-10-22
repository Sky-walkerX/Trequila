// Package planner is the service that turns a trips.requested event into an
// itinerary using the requesting user's own LLM key.
package planner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sky-walkerx/trequila/internal/ai"
	"github.com/sky-walkerx/trequila/internal/bus"
	"github.com/sky-walkerx/trequila/internal/events"
	"github.com/sky-walkerx/trequila/internal/llm"
	"github.com/sky-walkerx/trequila/internal/providerkeys"
	"github.com/sky-walkerx/trequila/internal/types"
)

// KeyResolver finds the LLM configuration for a user. *providerkeys.Store satisfies it.
type KeyResolver interface {
	Resolve(ctx context.Context, userID, overrideProvider string) (ai.ProviderConfig, error)
}

// ProviderFactory builds an LLM provider; the default is llm.NewProvider.
type ProviderFactory func(ctx context.Context, cfg ai.ProviderConfig) (ai.LLMProvider, error)

// Service implements the planner.
type Service struct {
	keys        KeyResolver
	pub         bus.Publisher
	newProvider ProviderFactory
	log         *slog.Logger
}

// Option customises a Service.
type Option func(*Service)

// WithProviderFactory replaces the provider constructor (used by tests).
func WithProviderFactory(f ProviderFactory) Option { return func(s *Service) { s.newProvider = f } }

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option { return func(s *Service) { s.log = l } }

// New creates the planner.
func New(keys KeyResolver, pub bus.Publisher, opts ...Option) *Service {
	s := &Service{keys: keys, pub: pub, newProvider: llm.NewProvider, log: slog.Default()}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Subscriptions lists what the planner consumes. The ack wait covers the slowest LLM
// call; a shorter one would redeliver the request while it is still being planned.
func (s *Service) Subscriptions() []bus.Subscription {
	return []bus.Subscription{{
		Durable:    "planner-v4",
		Type:       events.TripRequested,
		AckWait:    2 * time.Minute,
		MaxDeliver: 3,
		Handler:    s.handle,
	}}
}

func (s *Service) handle(ctx context.Context, e bus.Event) error {
	var trip types.TripRequest
	if err := e.Decode(&trip); err != nil {
		return err
	}
	log := s.log.With("trip_id", trip.RequestID, "destination", trip.Destination)
	log.Info("planning trip", "provider", trip.Provider, "model", trip.Model)

	cfg, err := s.keys.Resolve(ctx, trip.UserID, trip.Provider)
	if err != nil {
		if errors.Is(err, providerkeys.ErrNoKey) {
			log.Warn("the user has no key for this provider", "error", err)
			return s.fail(ctx, trip.RequestID, "No configured API key found.", err)
		}
		log.Warn("could not resolve the user's key; will retry", "error", err)
		return fmt.Errorf("resolve key: %w", err)
	}
	if trip.Model != "" {
		cfg.Model = trip.Model
	}

	provider, err := s.newProvider(ctx, cfg)
	if err != nil {
		log.Warn("could not build the LLM provider", "error", err)
		return s.fail(ctx, trip.RequestID, "Invalid provider or unsupported model.", err)
	}
	defer provider.Close()

	itinerary, err := provider.GenerateItinerary(ctx, trip)
	if err != nil {
		// Auth and billing failures never succeed on retry; check them first.
		if ai.IsAuthError(err) {
			log.Warn("provider rejected the key", "error", err)
			return s.fail(ctx, trip.RequestID, "API key invalid, expired, or out of credits.", err)
		}
		log.Warn("itinerary generation failed; will retry", "error", err)
		return fmt.Errorf("generate itinerary: %w", err)
	}

	out, err := bus.NewEvent(events.TripItinerary, events.ID(events.TripItinerary, trip.RequestID), events.ItineraryGenerated{
		RequestID:   trip.RequestID,
		UserID:      trip.UserID,
		Source:      trip.Source,
		Destination: trip.Destination,
		StartDate:   trip.StartDate,
		EndDate:     trip.EndDate,
		Itinerary:   itinerary,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return err
	}
	if err := s.pub.Publish(ctx, out); err != nil {
		return fmt.Errorf("publish itinerary: %w", err)
	}
	log.Info("itinerary generated", "days", len(itinerary))
	return nil
}

// fail publishes trips.failed and returns a permanent error so the request is not
// redelivered. If the failure event cannot be published the error is retryable: the
// trip must not be left without an outcome.
func (s *Service) fail(ctx context.Context, requestID, reason string, cause error) error {
	ev, err := bus.NewEvent(events.TripFailed, events.ID(events.TripFailed, requestID)+":planner", events.Failed{
		RequestID: requestID, Stage: "planner", Reason: reason,
	})
	if err != nil {
		return bus.Permanent(err)
	}
	if err := s.pub.Publish(ctx, ev); err != nil {
		return errors.Join(fmt.Errorf("publish failure event: %w", err), cause)
	}
	return bus.Permanent(cause)
}
