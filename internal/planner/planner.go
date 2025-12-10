// Package planner provides the trip planning service that orchestrates itinerary generation
package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/sky-walkerx/trequila/internal/ai"
	"github.com/sky-walkerx/trequila/internal/types"
)

// Service handles trip planning requests
type Service struct {
	generator ai.ItineraryGenerator
	js        jetstream.JetStream
	consumer  jetstream.Consumer
	config    Config
}

// Config holds configuration for the planner service
type Config struct {
	// ConsumerName is the durable consumer name
	ConsumerName string

	// InputSubject is the NATS subject to consume from
	InputSubject string

	// OutputSubject is the NATS subject to publish results to
	OutputSubject string

	// AckWait is how long to wait for message acknowledgment
	AckWait time.Duration

	// MaxDeliver is the maximum number of delivery attempts
	MaxDeliver int

	// MaxAckPending is the maximum number of unacknowledged messages
	MaxAckPending int
}

// DefaultConfig returns a sensible default configuration
// DefaultConfig returns a sensible default configuration
func DefaultConfig() Config {
    return Config{
        // Change the name to break the current "lock" immediately
        ConsumerName:  "planner-durable-v2", 

        InputSubject:  "trips.requested",
        OutputSubject: "trips.itinerary",
        
        // Change this from 120s to 10s for development!
        // If the planner crashes, NATS will retry in 10 seconds, not 2 minutes.
        AckWait:       10 * time.Second, 
        
        MaxDeliver:    3,
        MaxAckPending: 100,
    }
}

// New creates a new planner service
func New(js jetstream.JetStream, generator ai.ItineraryGenerator, cfg Config) (*Service, error) {
	return &Service{
		generator: generator,
		js:        js,
		config:    cfg,
	}, nil
}

// Start begins processing trip requests
func (s *Service) Start(ctx context.Context, streamName string) error {
	stream, err := s.js.Stream(ctx, streamName)
	if err != nil {
		return fmt.Errorf("planner: failed to get stream %s: %w", streamName, err)
	}

	consumer, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       s.config.ConsumerName,
		FilterSubject: s.config.InputSubject,
		AckPolicy:     jetstream.AckExplicitPolicy,
		MaxDeliver:    s.config.MaxDeliver,
		AckWait:       s.config.AckWait,
		MaxAckPending: s.config.MaxAckPending,
	})
	if err != nil {
		return fmt.Errorf("planner: failed to create consumer: %w", err)
	}

	s.consumer = consumer
	log.Printf("Planner service started. Consuming from %s", s.config.InputSubject)

	return s.processLoop(ctx)
}

// processLoop is the main message processing loop
func (s *Service) processLoop(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			log.Println("Planner service shutting down...")
			return ctx.Err()
		default:
		}

		if err := s.processNextMessage(ctx); err != nil {
			log.Printf("Error processing message: %v", err)
		}
	}
}

// processNextMessage fetches and processes the next message from the queue
func (s *Service) processNextMessage(ctx context.Context) error {
	msgs, err := s.consumer.Fetch(1, jetstream.FetchMaxWait(5*time.Second))
	if err != nil {
		if err == context.DeadlineExceeded || err == jetstream.ErrNoMessages {
			return nil // Normal timeout, no messages available
		}
		return fmt.Errorf("fetch error: %w", err)
	}

	msg, ok := <-msgs.Messages()
	if !ok {
		return nil
	}

	return s.handleMessage(ctx, msg)
}

// handleMessage processes a single trip request message
func (s *Service) handleMessage(ctx context.Context, msg jetstream.Msg) error {
	var event types.TripEvent
	if err := json.Unmarshal(msg.Data(), &event); err != nil {
		log.Printf("Invalid payload, acknowledging to skip: %v", err)
		return msg.Ack()
	}

	trip := event.Data
	log.Printf("Processing trip request %s (%s -> %s, %s to %s)",
		trip.RequestID, trip.Source, trip.Destination, trip.StartDate, trip.EndDate)

	// Generate itinerary using AI
	itinerary, err := s.generator.GenerateItinerary(ctx, trip)
	if err != nil {
		log.Printf("Failed to generate itinerary for %s: %v", trip.RequestID, err)
		return msg.Nak() // Negative ack for retry
	}

	// Build and publish the result event
	if err := s.publishItinerary(ctx, trip, itinerary); err != nil {
		log.Printf("Failed to publish itinerary for %s: %v", trip.RequestID, err)
		return err
	}

	if err := msg.Ack(); err != nil {
		return fmt.Errorf("ack error: %w", err)
	}

	log.Printf("Successfully processed request %s", trip.RequestID)
	return nil
}

// publishItinerary publishes the generated itinerary event
func (s *Service) publishItinerary(ctx context.Context, trip types.TripRequest, itinerary []types.DayPlan) error {
	event := types.ItineraryGeneratedEvent{
		Type: "ItineraryGenerated",
		Data: types.ItineraryGeneratedEventData{
			RequestID:   trip.RequestID,
			UserID:      trip.UserID,
			Source:      trip.Source,
			Destination: trip.Destination,
			StartDate:   trip.StartDate,
			EndDate:     trip.EndDate,
			Itinerary:   itinerary,
			FlightSearchParams: types.FlightSearchParams{
				Origin:        trip.Source,
				Destination:   trip.Destination,
				DepartureDate: trip.StartDate,
				ReturnDate:    trip.EndDate,
			},
			HotelSearchParams: types.HotelSearchParams{
				Location: trip.Destination,
				CheckIn:  trip.StartDate,
				CheckOut: trip.EndDate,
			},
			GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		},
	}

	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal event: %w", err)
	}

	pubCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	_, err = s.js.Publish(pubCtx, s.config.OutputSubject, data,
		jetstream.WithMsgID(trip.RequestID),
	)
	if err != nil {
		return fmt.Errorf("failed to publish: %w", err)
	}

	return nil
}

// Stop gracefully stops the planner service
func (s *Service) Stop() error {
	if s.generator != nil {
		return s.generator.Close()
	}
	return nil
}
