// Package memory is an in-process bus for tests. It mirrors the delivery rules of
// the real adapters: durable subscriptions replay everything published so far,
// publishing an ID twice delivers once (like the Nats-Msg-Id window), transient
// errors are redelivered up to MaxDeliver, permanent errors are not.
package memory

import (
	"context"
	"errors"
	"sync"

	"github.com/sky-walkerx/trequila/internal/bus"
)

const defaultMaxDeliver = 3

// Bus is safe for concurrent use.
type Bus struct {
	mu      sync.Mutex
	cond    *sync.Cond
	log     []bus.Event
	seen    map[string]bool
	cursors map[string]int // durable -> index of the next event to consider
}

// New returns an empty bus.
func New() *Bus {
	b := &Bus{seen: map[string]bool{}, cursors: map[string]int{}}
	b.cond = sync.NewCond(&b.mu)
	return b
}

// Publish appends the event unless an event with the same ID was already published.
func (b *Bus) Publish(_ context.Context, e bus.Event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.seen[e.ID] {
		return nil
	}
	b.seen[e.ID] = true
	b.log = append(b.log, e)
	b.cond.Broadcast()
	return nil
}

// Events returns a copy of everything published so far, for assertions.
func (b *Bus) Events() []bus.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]bus.Event(nil), b.log...)
}

// Subscribe delivers matching events one at a time until ctx is cancelled.
func (b *Bus) Subscribe(ctx context.Context, s bus.Subscription) error {
	if s.Durable == "" || s.Type == "" || s.Handler == nil {
		return errors.New("memory bus: subscription needs a durable name, a type and a handler")
	}
	max := s.MaxDeliver
	if max <= 0 {
		max = defaultMaxDeliver
	}

	go func() {
		<-ctx.Done()
		b.mu.Lock()
		b.cond.Broadcast()
		b.mu.Unlock()
	}()

	for {
		b.mu.Lock()
		var (
			ev  bus.Event
			idx int
		)
		for {
			if ctx.Err() != nil {
				b.mu.Unlock()
				return nil
			}
			idx = b.cursors[s.Durable]
			for idx < len(b.log) && b.log[idx].Type != s.Type {
				idx++
			}
			if idx < len(b.log) {
				ev = b.log[idx]
				break
			}
			b.cursors[s.Durable] = idx
			b.cond.Wait()
		}
		b.mu.Unlock()

		for attempt := 1; attempt <= max; attempt++ {
			err := s.Handler(ctx, ev)
			if err == nil || bus.IsPermanent(err) || ctx.Err() != nil {
				break
			}
		}

		b.mu.Lock()
		b.cursors[s.Durable] = idx + 1
		b.mu.Unlock()
	}
}
