package main

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	natshelper "github.com/sky-walkerx/trequila/internal/nats"
)

func main() {
	// Initialize JetStream connection
	js := natshelper.ConnectJetStream(nats.DefaultURL)

	ctx := context.Background()

	// Get reference to the TRIPS stream
	stream, err := js.Stream(ctx, "TRIPS")
	if err != nil {
		log.Fatalf("Failed to get stream: %v", err)
	}

	// Create or update a durable consumer
	// Durable consumers remember their position even after disconnecting
	consumer, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       "planner-durable",           // Consumer name (persisted)
		FilterSubject: "trips.requested",           // Only consume messages from this subject
		AckPolicy:     jetstream.AckExplicitPolicy, // Must explicitly acknowledge messages
		MaxDeliver:    3,                           // Max redelivery attempts
		AckWait:       30 * time.Second,            // Wait 30s for ack before redelivering
		MaxAckPending: 100,                         // Max unacked messages
	})
	if err != nil {
		log.Fatalf("Failed to create consumer: %v", err)
	}

	log.Println("Planner service started. Processing trip requests...")

	// Main processing loop
	for {
		// Fetch next message with timeout
		msgs, err := consumer.Fetch(1,
			jetstream.FetchMaxWait(5*time.Second),
		)

		if err != nil {
			// Timeout is normal when no messages are available
			if err == context.DeadlineExceeded || err == jetstream.ErrNoMessages {
				continue
			}
			log.Printf("Fetch error: %v", err)
			time.Sleep(1 * time.Second)
			continue
		}

		// Get the message from the channel
		m, ok := <-msgs.Messages()
		if !ok {
			continue
		}

		// Parse the event payload
		var ev map[string]interface{}
		if err := json.Unmarshal(m.Data(), &ev); err != nil {
			log.Printf("Invalid payload: %v", err)
			_ = m.Ack() // Acknowledge bad messages to prevent reprocessing
			continue
		}

		// Extract request details
		data := ev["data"].(map[string]interface{})
		reqID := data["request_id"].(string)
		source := data["source"].(string)
		destination := data["destination"].(string)
		startDate := data["start_date"].(string)
		endDate := data["end_date"].(string)

		log.Printf("Processing trip request %s (%s -> %s, %s to %s)",
			reqID, source, destination, startDate, endDate)

		// Simulate AI processing (replace with actual logic)
		time.Sleep(2 * time.Second)

		// Create itinerary generated event
		itEvent := map[string]interface{}{
			"type": "ItineraryGenerated",
			"data": map[string]interface{}{
				"request_id":  reqID,
				"source":      source,
				"destination": destination,
				"start_date":  startDate,
				"end_date":    endDate,
				"steps": []string{
					"Day 1: Arrive & relax",
					"Day 2: City tour",
					"Day 3: Beach day",
				},
				"flight_search_params": map[string]interface{}{
					"origin":         source,
					"destination":    destination,
					"departure_date": startDate,
					"return_date":    endDate,
				},
				"hotel_search_params": map[string]interface{}{
					"location":  destination,
					"check_in":  startDate,
					"check_out": endDate,
				},
				"generated_at": time.Now().UTC().Format(time.RFC3339),
			},
		}

		out, _ := json.Marshal(itEvent)

		// Publish itinerary event to a different subject
		pubCtx, cancelPub := context.WithTimeout(ctx, 3*time.Second)
		ack, err := js.Publish(pubCtx, "trips.itinerary", out,
			jetstream.WithMsgID(reqID), // Prevents duplicate itineraries
		)
		cancelPub()

		if err != nil {
			log.Printf("Failed to publish itinerary: %v", err)
			// Don't ack the original message - it will be redelivered
			continue
		}

		// Acknowledge the original message (removes it from the queue)
		if err := m.Ack(); err != nil {
			log.Printf("Ack error: %v", err)
		} else {
			log.Printf("Successfully processed request %s (itinerary seq: %d)", reqID, ack.Sequence)
		}
	}
}
