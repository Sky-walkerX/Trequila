// Package bus is the transport-neutral event contract the services speak.
//
// Services publish Events and subscribe with Handlers; they never import a broker
// client. Adapters (internal/bus/natsbus, internal/bus/memory) carry the delivery
// semantics:
//
//   - a handler that returns nil acknowledges the event;
//   - a handler that returns an error wrapped with Permanent acknowledges it too (it
//     will never succeed, so redelivery would only loop) and the adapter logs it;
//   - any other error, or a panic, asks for redelivery, up to Subscription.MaxDeliver;
//   - delivery is at least once, so handlers must be idempotent (see internal/inbox).
package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Event is one message on the bus. Type doubles as the routing key (a NATS subject
// today). Data is the JSON payload; TraceParent is reserved for trace propagation.
type Event struct {
	ID          string          `json:"id"`
	Type        string          `json:"type"`
	TraceParent string          `json:"traceparent,omitempty"`
	Data        json.RawMessage `json:"data"`
}

// NewEvent marshals payload into an Event. An empty id generates one. Use a
// deterministic id (for example "trips.itinerary:<trip>") when a redelivered input
// should produce an event that the broker and consumers recognise as the same one.
func NewEvent(typ, id string, payload any) (Event, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return Event{}, fmt.Errorf("bus: marshal %s payload: %w", typ, err)
	}
	if id == "" {
		id = uuid.NewString()
	}
	return Event{ID: id, Type: typ, Data: data}, nil
}

// Decode unmarshals the payload into v. A payload that does not decode will never
// decode, so the error is permanent.
func (e Event) Decode(v any) error {
	if err := json.Unmarshal(e.Data, v); err != nil {
		return Permanent(fmt.Errorf("decode %s event %s: %w", e.Type, e.ID, err))
	}
	return nil
}

// Publisher sends events.
type Publisher interface {
	Publish(ctx context.Context, e Event) error
}

// Handler processes one delivery.
type Handler func(ctx context.Context, e Event) error

// Subscription describes a durable consumer of one event type.
type Subscription struct {
	Durable    string        // stable name; bump its suffix when the config changes
	Type       string        // event type to receive
	AckWait    time.Duration // how long a delivery may run before it is redelivered
	MaxDeliver int           // delivery attempts before the event is dropped (0 = adapter default)
	Handler    Handler
}

// Bus is a Publisher that can also deliver events to subscriptions.
type Bus interface {
	Publisher
	// Subscribe delivers events to s.Handler until ctx is cancelled.
	Subscribe(ctx context.Context, s Subscription) error
}

// Pinger is implemented by buses that can report their own health.
type Pinger interface {
	Ping(ctx context.Context) error
}

type permanentError struct{ err error }

func (p *permanentError) Error() string { return p.err.Error() }
func (p *permanentError) Unwrap() error { return p.err }

// Permanent marks err as one that redelivery cannot fix.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanentError{err: err}
}

// IsPermanent reports whether err, or anything it wraps, was marked Permanent.
func IsPermanent(err error) bool {
	var p *permanentError
	return errors.As(err, &p)
}
