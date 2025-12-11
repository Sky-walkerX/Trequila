// cmd/imager/main.go
package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sky-walkerx/trequila/internal/images"
	natshelper "github.com/sky-walkerx/trequila/internal/nats"
	"github.com/sky-walkerx/trequila/internal/types"
)

type ImagesFoundEvent struct {
	Type string `json:"type"`
	Data struct {
		RequestID string         `json:"request_id"`
		Images    []images.Image `json:"images"`
	} `json:"data"`
}

func main() {
	apiKey := os.Getenv("PEXELS_API_KEY")
	redisAddr := os.Getenv("REDIS_ADDR")
	if apiKey == "" {
		log.Fatal("PEXELS_API_KEY required")
	}

	// Initialize Services
	imgSvc := images.NewService(redisAddr, apiKey)
	js := natshelper.ConnectJetStream(os.Getenv("NATS_URL"))

	// Consume TRIP REQUESTS (Parallel to Planner)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream, _ := js.Stream(ctx, "TRIPS")
	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       "imager-worker-v1",
		FilterSubject: "trips.requested",
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       30 * time.Second,
	})
	if err != nil {
		log.Fatal(err)
	}

	log.Println("Imager Service Started...")

	// Graceful shutdown
	go func() {
		s := make(chan os.Signal, 1)
		signal.Notify(s, syscall.SIGINT, syscall.SIGTERM)
		<-s
		cancel()
	}()

	for {
		msgs, err := cons.Fetch(1, jetstream.FetchMaxWait(5*time.Second))
		if err != nil {
			continue
		}

		for msg := range msgs.Messages() {
			var event types.TripEvent
			if err := json.Unmarshal(msg.Data(), &event); err != nil {
				msg.Ack()
				continue
			}

			reqID := event.Data.RequestID
			dest := event.Data.Destination
			log.Printf("Fetching images for %s (%s)", reqID, dest)

			imgs, err := imgSvc.GetImages(ctx, dest)
			if err != nil {
				log.Printf("Image fetch failed: %v", err)
				msg.Nak() // Retry later (rate limit might be reset)
				continue
			}

			// Publish Results
			resEvent := ImagesFoundEvent{
				Type: "ImagesFound",
			}
			resEvent.Data.RequestID = reqID
			resEvent.Data.Images = imgs

			data, _ := json.Marshal(resEvent)
			js.Publish(ctx, "trips.images", data)

			msg.Ack()
			log.Printf("Images found for %s", reqID)
		}
	}
}