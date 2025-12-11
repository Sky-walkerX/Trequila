package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/sky-walkerx/trequila/internal/geo"
	natshelper "github.com/sky-walkerx/trequila/internal/nats"
	"github.com/sky-walkerx/trequila/internal/types"
)

func main() {
	apiKey := os.Getenv("GEOAPIFY_API_KEY")
	js := natshelper.ConnectJetStream(os.Getenv("NATS_URL"))
	geoSvc := geo.NewService(apiKey)

	// Consume TRIP REQUESTS
	ctx := context.Background()
	stream, _ := js.Stream(ctx, "TRIPS")
	cons, _ := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       "places-worker-v1",
		FilterSubject: "trips.requested",
		AckPolicy:     jetstream.AckExplicitPolicy,
	})

	log.Println("Places Service Started...")

	for {
		msgs, err := cons.Fetch(1, jetstream.FetchMaxWait(5*time.Second))
		if err != nil { continue }

		for msg := range msgs.Messages() {
			var event types.TripEvent
			json.Unmarshal(msg.Data(), &event)
			dest := event.Data.Destination
			
			log.Printf("Finding places for %s...", dest)

			// 1. Geocode
			lat, lon, err := geoSvc.GetCoordinates(dest)
			if err != nil {
				log.Printf("Geocode failed: %v", err)
				msg.Nak()
				continue
			}

			// 2. Fetch Categories (Parallelly or Sequentially)
			hotels, _ := geoSvc.SearchPlaces(lat, lon, "accommodation.hotel", 5)
			restaurants, _ := geoSvc.SearchPlaces(lat, lon, "catering.restaurant", 5)
			attractions, _ := geoSvc.SearchPlaces(lat, lon, "tourism.attraction", 5)

			// 3. Construct Payload
			result := map[string]interface{}{
				"hotels":      hotels,
				"restaurants": restaurants,
				"attractions": attractions,
			}

			payload := map[string]interface{}{
				"request_id": event.Data.RequestID,
				"places":     result,
			}
			
			// 4. Publish
			data, _ := json.Marshal(map[string]interface{}{
				"type": "PlacesFound",
				"data": payload,
			})
			
			js.Publish(ctx, "trips.places", data)
			msg.Ack()
			log.Printf("Places found for %s", dest)
		}
	}
}