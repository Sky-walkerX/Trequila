package route

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/sky-walkerx/trequila/internal/geo"
	"github.com/sky-walkerx/trequila/internal/types"
)

// SubjectRequested is published once a trip has an itinerary worth routing, and
// again whenever the user asks for the route to be recomputed. The publisher
// includes the places it already knows so the worker geocodes as little as
// possible — it has no database of its own.
const SubjectRequested = "trips.route_requested"

// SubjectResult carries the computed route back to the API, which persists it
// and forwards it to any attached SSE stream.
const SubjectResult = "trips.route"

// geocodeBudget bounds one job's wall-clock time. Lookups are rate-limited to
// about one per second, so a long trip full of unfamiliar names would otherwise
// keep the message in flight past its AckWait and be redelivered mid-flight.
const geocodeBudget = 3 * time.Minute

// ackWait must exceed geocodeBudget plus the in-flight request that may be
// running when the budget expires.
const ackWait = 5 * time.Minute

// RequestData is the payload of SubjectRequested.
type RequestData struct {
	RequestID   string          `json:"request_id"`
	Destination string          `json:"destination"`
	Itinerary   []types.DayPlan `json:"itinerary"`
	Places      []geo.Place     `json:"places"`
}

// RequestEnvelope matches the NATS envelope convention used across the stream.
type RequestEnvelope struct {
	Type string      `json:"type"`
	Data RequestData `json:"data"`
}

// ResultData is the payload of SubjectResult.
type ResultData struct {
	RequestID string `json:"request_id"`
	Route     Data   `json:"route"`
}

// ResultEnvelope wraps ResultData for publication.
type ResultEnvelope struct {
	Type string     `json:"type"`
	Data ResultData `json:"data"`
}

// PublishRequest asks for a trip's route to be computed.
func PublishRequest(ctx context.Context, js jetstream.JetStream, data RequestData) error {
	payload, err := json.Marshal(RequestEnvelope{Type: "RouteRequested", Data: data})
	if err != nil {
		return err
	}
	_, err = js.Publish(ctx, SubjectRequested, payload)
	return err
}

// StartWorker consumes route requests until ctx is cancelled.
func StartWorker(ctx context.Context, js jetstream.JetStream, geocoder Geocoder) {
	var stream jetstream.Stream
	var err error
	for i := 0; i < 30; i++ {
		if stream, err = js.Stream(ctx, "TRIPS"); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(1 * time.Second):
		}
	}
	if err != nil {
		log.Printf("❌ Route: failed to find TRIPS stream: %v", err)
		return
	}

	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       "route-worker-v1",
		FilterSubject: SubjectRequested,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       ackWait,
		MaxDeliver:    3,
	})
	if err != nil {
		log.Printf("❌ Route: consumer init failed: %v", err)
		return
	}

	log.Println("🧭 Route Worker Started")

	for {
		select {
		case <-ctx.Done():
			log.Println("Route: worker shutting down...")
			return
		default:
		}

		msgs, err := cons.Fetch(1, jetstream.FetchMaxWait(5*time.Second))
		if err != nil {
			continue
		}

		for msg := range msgs.Messages() {
			var envelope RequestEnvelope
			if err := json.Unmarshal(msg.Data(), &envelope); err != nil {
				log.Printf("⚠️ Route: parse error: %v", err)
				msg.Ack() // bad data will never parse; acking avoids a redelivery loop
				continue
			}

			event := envelope.Data
			if event.RequestID == "" || len(event.Itinerary) == 0 {
				msg.Ack()
				continue
			}

			locator := NewPlaceLocator(event.Destination, event.Places, geocoder).
				WithDeadline(time.Now().Add(geocodeBudget))
			computed := Build(event.Itinerary, locator)

			if err := publishResult(ctx, js, event.RequestID, computed); err != nil {
				log.Printf("Route: publish failed for %s: %v", event.RequestID, err)
				msg.Nak()
				continue
			}
			log.Printf("Route: computed %d days for %s", len(computed.Days), event.RequestID)
			msg.Ack()
		}
	}
}

func publishResult(ctx context.Context, js jetstream.JetStream, requestID string, data Data) error {
	payload, err := json.Marshal(ResultEnvelope{
		Type: "RouteComputed",
		Data: ResultData{RequestID: requestID, Route: data},
	})
	if err != nil {
		return err
	}
	_, err = js.Publish(ctx, SubjectResult, payload)
	return err
}
