package vibes

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/sky-walkerx/trequila/internal/nats"
)

// IncomingPlace matches the JSON structure from your Places Worker
type IncomingPlace struct {
	Name     string  `json:"name"`
	Category string  `json:"category"`
	Lat      float64 `json:"lat"`
	Lon      float64 `json:"lon"`
}

type PlacesEvent struct {
	RequestID string          `json:"request_id"`
	Places    []IncomingPlace `json:"places"`
}

// StartWorker is called by the Monolith to start this background job
func StartWorker(natsURL, apiKey string) {
	// 1. Connect to NATS
	// Note: We use the helper from internal/nats to ensure consistent connection logic
	// If you passed the raw URL, we connect here.
	js, _ := nats.ConnectJetStream(natsURL, "", "") 
	ctx := context.Background()

	// 2. Setup Consumer (Listen for Places)
	// We wait until the TRIPS stream exists (created by other workers)
	var stream jetstream.Stream
	var err error
	for i := 0; i < 10; i++ {
		stream, err = js.Stream(ctx, "TRIPS")
		if err == nil { break }
		time.Sleep(2 * time.Second)
	}

	if err != nil {
		log.Printf("❌ Vibes: Failed to find TRIPS stream: %v", err)
		return
	}

	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       "vibes-worker-monolith",
		FilterSubject: "trips.places", // Listen to the Places worker output
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	if err != nil {
		log.Printf("❌ Vibes: Consumer init failed: %v", err)
		return
	}

	log.Println("✨ Vibes Worker (AI Powered) Started...")

	// 3. The Infinite Loop
	for {
		msgs, err := cons.Fetch(1)
		if err != nil {
			// No messages available, sleep briefly
			time.Sleep(1 * time.Second)
			continue
		}

		for msg := range msgs.Messages() {
			var event PlacesEvent
			if err := json.Unmarshal(msg.Data(), &event); err != nil {
				log.Printf("⚠️ Vibes parse error: %v", err)
				msg.Ack()
				continue
			}

			log.Printf("Vibes: Analyzing %d places for %s...", len(event.Places), event.RequestID)

			// Process each place found
			for _, p := range event.Places {
				// A. Weather Check (Free Open-Meteo)
				weatherSummary, _ := GetWeather(p.Lat, p.Lon)

				// B. Super Analysis (Gemini AI)
				insights := AnalyzePlaceWithAI(apiKey, p.Name, p.Category)

				// C. Publish Result
				payload := map[string]interface{}{
					"request_id":   event.RequestID,
					"place_name":   p.Name,
					"weather":      weatherSummary,
					"safety_score": insights.SafetyScore,
					"safety_tip":   insights.SafetyTip,
					"crowds":       insights.CrowdLevel,
					"best_time":    insights.BestTime,
					"duration":     insights.Duration,
					"photo_tip":    insights.PhotoTip,
				}

				data, _ := json.Marshal(payload)
				// Publish to "trips.vibes.update" (Frontend listens to this via SSE)
				js.Publish(ctx, "trips.vibes.update", data)
				
				log.Printf("✅ Vibe check sent for: %s", p.Name)
			}
			msg.Ack()
		}
	}
}