package tripevents

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

const channel = "trip_events"

// Hub turns Postgres NOTIFY messages into per-trip doorbells.
//
// One process holds one dedicated connection that LISTENs on the trip_events channel;
// any number of SSE connections subscribe to individual trips. A doorbell is a
// channel with room for a single pending ring: it says "something changed, query the
// log again", never what changed, so a slow reader can never block the hub or lose
// an event.
type Hub struct {
	connString string
	log        *slog.Logger

	mu    sync.Mutex
	trips map[string]map[chan struct{}]struct{}

	readyOnce sync.Once
	ready     chan struct{}
}

// NewHub creates a hub that will listen using connString.
func NewHub(connString string, log *slog.Logger) *Hub {
	if log == nil {
		log = slog.Default()
	}
	return &Hub{
		connString: connString,
		log:        log,
		trips:      map[string]map[chan struct{}]struct{}{},
		ready:      make(chan struct{}),
	}
}

// Ready is closed once the hub is listening for the first time.
func (h *Hub) Ready() <-chan struct{} { return h.ready }

// Subscribe registers a doorbell for tripID. Call the returned function to remove it.
func (h *Hub) Subscribe(tripID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	if h.trips[tripID] == nil {
		h.trips[tripID] = map[chan struct{}]struct{}{}
	}
	h.trips[tripID][ch] = struct{}{}
	h.mu.Unlock()

	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(h.trips[tripID], ch)
		if len(h.trips[tripID]) == 0 {
			delete(h.trips, tripID)
		}
	}
}

func (h *Hub) ring(tripID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.trips[tripID] {
		select {
		case ch <- struct{}{}:
		default: // a ring is already pending
		}
	}
}

func (h *Hub) ringAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, set := range h.trips {
		for ch := range set {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}
}

// Run listens until ctx is cancelled, reconnecting with backoff when the connection
// drops. After every (re)connect it rings every doorbell, because notifications sent
// while the connection was down are gone and readers must re-query to catch up.
func (h *Hub) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := h.listen(ctx)
		if ctx.Err() != nil {
			return
		}
		h.log.Warn("trip event listener lost its connection; reconnecting", "error", err, "retry_in", backoff)
		h.ringAll()
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (h *Hub) listen(ctx context.Context) error {
	conn, err := pgx.Connect(ctx, h.connString)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())

	if _, err := conn.Exec(ctx, "LISTEN "+channel); err != nil {
		return err
	}
	h.ringAll() // anything appended before the LISTEN took effect
	h.readyOnce.Do(func() { close(h.ready) })

	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		h.ring(n.Payload)
	}
}
