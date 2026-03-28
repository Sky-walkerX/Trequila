package vibes

import (
    "context"
    "encoding/json"
    "log"
    "time"

    "github.com/nats-io/nats.go/jetstream"
)

// Incoming structures from the "Places" worker
type IncomingPlace struct {
    Name     string  `json:"name"`
    Category string  `json:"category"`
    Lat      float64 `json:"lat"`
    Lon      float64 `json:"lon"`
}

// 1. Match the "Data" part of the producer's JSON
type PlacesData struct {
    RequestID string                      `json:"request_id"`
    Places    map[string][]IncomingPlace  `json:"places"`
}

// 2. Define the Envelope that matches the Producer (NATS message structure)
type PlacesEnvelope struct {
    Type string     `json:"type"`
    Data PlacesData `json:"data"`
}

// Final output structure to be saved in DB
type VibeData struct {
    Weather     string `json:"weather"` // Short summary: "24°C ☀️"
    SafetyScore int    `json:"safety_score"`
    SafetyTip   string `json:"safety_tip"`
    CrowdLevel  string `json:"crowd_level"`
    BestTime    string `json:"best_time"`
    Duration    string `json:"duration"`
    PhotoTip    string `json:"photo_tip"`
}

func StartWorker(ctx context.Context, js jetstream.JetStream, apiKey string) {

    // --- STEP 1: Find the Stream ---
    var stream jetstream.Stream
    var err error
    for i := 0; i < 30; i++ {
        stream, err = js.Stream(ctx, "TRIPS")
        if err == nil {
            break
        }
        time.Sleep(1 * time.Second)
    }
    if err != nil {
        log.Printf("❌ Vibes: Failed to find TRIPS stream: %v", err)
        return
    }

    // --- STEP 2: Create the Consumer ---
    cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
        Durable:       "vibes-worker-v1",
        FilterSubject: "trips.places", // Triggered when Places worker finishes
        AckPolicy:     jetstream.AckExplicitPolicy,
        AckWait:       60 * time.Second, // Give AI time to think
    })
    if err != nil {
        log.Printf("❌ Vibes: Consumer init failed: %v", err)
        return
    }

    log.Println("✨ Vibes Worker Started (Batch Mode)")

    // --- STEP 3: Start the Processing Loop ---
    for {
        select {
        case <-ctx.Done():
            log.Println("Vibes: Worker shutting down...")
            return
        default:
        }

        // Fetch messages
        msgs, err := cons.Fetch(1, jetstream.FetchMaxWait(5*time.Second))
        if err != nil {
            continue
        }

        for msg := range msgs.Messages() {
            // --- FIX APPLIED HERE ---
            // We use the Envelope struct to safely unwrap the message
            var envelope PlacesEnvelope
            if err := json.Unmarshal(msg.Data(), &envelope); err != nil {
                log.Printf("⚠️ Vibes parse error: %v", err)
                msg.Ack() // Ack bad data so we don't get stuck in a loop
                continue
            }

            // Extract the actual data we need
            event := envelope.Data
            
            log.Printf("Vibes: Processing request %s", event.RequestID)

            // 1. Flatten places for Batch AI processing
            var placesToAnalyze []PlaceInfo
            // We also map them to access lat/lon easily later
            placeMap := make(map[string]IncomingPlace)

            for _, list := range event.Places {
                for _, p := range list {
                    placesToAnalyze = append(placesToAnalyze, PlaceInfo{
                        Name:     p.Name,
                        Category: p.Category,
                    })
                    placeMap[p.Name] = p
                }
            }

            if len(placesToAnalyze) == 0 {
                msg.Ack()
                continue
            }

            // 2. Call AI (ONE call for all places)
            aiResults := BatchAnalyzePlaces(ctx, apiKey, placesToAnalyze)

            // 3. Merge AI Data + Weather
            finalOutput := make(map[string]VibeData)

            for name, insights := range aiResults {
                originalPlace, exists := placeMap[name]
                if !exists {
                    continue
                }

                // Quick weather check (Weather API is fast/cheap)
                weatherSummary, _ := GetWeather(originalPlace.Lat, originalPlace.Lon)

                finalOutput[name] = VibeData{
                    Weather:     weatherSummary,
                    SafetyScore: insights.SafetyScore,
                    SafetyTip:   insights.SafetyTip,
                    CrowdLevel:  insights.CrowdLevel,
                    BestTime:    insights.BestTime,
                    Duration:    insights.Duration,
                    PhotoTip:    insights.PhotoTip,
                }
            }

            // 4. Publish "trips.vibes"
            payload := map[string]interface{}{
                "type": "VibesGenerated",
                "data": map[string]interface{}{
                    "request_id": event.RequestID,
                    "vibes":      finalOutput,
                },
            }

            data, _ := json.Marshal(payload)

            // Note: Subject is "trips.vibes" to match other workers
            if _, err := js.Publish(ctx, "trips.vibes", data); err != nil {
                log.Printf("Vibes: Publish failed: %v", err)
                msg.Nak() // Retry later
            } else {
                log.Printf("✅ Vibes generated for %s", event.RequestID)
                msg.Ack()
            }
        }
    }
}