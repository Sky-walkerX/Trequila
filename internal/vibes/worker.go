package vibes

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

type IncomingPlace struct {
	Name     string  `json:"name"`
	Category string  `json:"category"`
	Lat      float64 `json:"lat"`
	Lon      float64 `json:"lon"`
}

// FIX 1: Update struct to match the "Map" structure sent by Places worker
type PlacesEvent struct {
	RequestID string                      `json:"request_id"`
	Places    map[string][]IncomingPlace  `json:"places"` // Changed from [] to map[string][]
}

func StartWorker(js jetstream.JetStream, apiKey string) {
	ctx := context.Background()

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
		FilterSubject: "trips.places", 
		AckPolicy:     jetstream.AckExplicitPolicy,
	})
	if err != nil {
		log.Printf("❌ Vibes: Consumer init failed: %v", err)
		return
	}

	log.Println("✨ Vibes Worker (AI Powered) Started...")

	for {
		msgs, err := cons.Fetch(1)
		if err != nil {
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

            // FIX 2: Flatten the map. Count total places first.
            totalPlaces := 0
            for _, list := range event.Places {
                totalPlaces += len(list)
            }
			log.Printf("Vibes: Analyzing %d places for %s...", totalPlaces, event.RequestID)

			// FIX 3: Iterate through categories (hotels, restaurants, etc.)
			for _, placeList := range event.Places {
                for _, p := range placeList {
                    
                    // A. Weather Check
                    weatherSummary, _ := GetWeather(p.Lat, p.Lon)

                    // B. Super Analysis
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
                    js.Publish(ctx, "trips.vibes.update", data)
                    
                    log.Printf("✅ Vibe check sent for: %s", p.Name)
                }
            }
			msg.Ack()
		}
	}
}