package api

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/sky-walkerx/trequila/internal/metrics"
	"github.com/sky-walkerx/trequila/internal/tripevents"
)

func (s *Service) handleStreamTripAuth(w http.ResponseWriter, r *http.Request, userID, tripID string) {
	var exists bool
	s.db.QueryRowContext(r.Context(),
		`SELECT EXISTS(SELECT 1 FROM trips WHERE request_id = $1 AND user_id = $2)`, tripID, userID).Scan(&exists)
	if !exists {
		writeError(w, http.StatusNotFound, "trip not found")
		return
	}
	s.streamTrip(w, r, tripID)
}

// lastEventID reads where the client wants to resume from: the standard Last-Event-ID
// header that EventSource and fetch-event-source send on reconnect, or a lastEventId
// query parameter for clients that cannot set headers.
func lastEventID(r *http.Request) int64 {
	v := r.Header.Get("Last-Event-ID")
	if v == "" {
		v = r.URL.Query().Get("lastEventId")
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil || id < 0 {
		return 0
	}
	return id
}

// streamTrip serves a trip's event log as Server-Sent Events: replay what the client
// has not seen, then follow new events until the trip finishes.
//
// The doorbell is registered before the first replay query. An event committed after
// the query but before the wait still rings it, so nothing is missed, which a "read
// the database, then subscribe" order cannot guarantee.
//
// The stream ends when the trip fails, when the itinerary and places have both
// arrived, when places are still missing streamPlacesGrace after the itinerary, or at
// the hard timeout. Routing is deliberately not part of completion: it can finish after
// the rest, and GET /api/trips/{id} serves it then.
func (s *Service) streamTrip(w http.ResponseWriter, r *http.Request, tripID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	ctx := r.Context()

	var bell <-chan struct{}
	if s.hub != nil {
		b, cancel := s.hub.Subscribe(tripID)
		defer cancel()
		bell = b
	} else {
		// No LISTEN connection (tests, degraded mode): fall back to polling.
		t := time.NewTicker(time.Second)
		defer t.Stop()
		c := make(chan struct{}, 1)
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					select {
					case c <- struct{}{}:
					default:
					}
				}
			}
		}()
		bell = c
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	fmt.Fprintf(w, "event: connected\ndata: {\"request_id\":%q,\"message\":\"Stream connected\"}\n\n", tripID)
	flusher.Flush()

	metrics.SSEConnectionsActive.Inc()
	defer metrics.SSEConnectionsActive.Dec()

	complete := func() {
		fmt.Fprint(w, "event: complete\ndata: {\"message\":\"All data received\"}\n\n")
		flusher.Flush()
	}

	last := lastEventID(r)
	var progress streamProgress
	var grace <-chan time.Time
	hard := time.NewTimer(s.streamHardTimeout)
	defer hard.Stop()
	heartbeat := time.NewTicker(s.streamHeartbeat)
	defer heartbeat.Stop()

	for {
		evs, err := tripevents.After(ctx, s.db, tripID, last)
		if err != nil {
			if ctx.Err() == nil {
				s.log.Error("stream query failed", "trip_id", tripID, "error", err)
				fmt.Fprint(w, "event: failed\ndata: {\"type\":\"failed\",\"reason\":\"The server could not read this trip's progress. Please reconnect.\"}\n\n")
				flusher.Flush()
			}
			return
		}
		for _, ev := range evs {
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.ID, ev.Type, ev.Data)
			flusher.Flush()
			last = ev.ID
			switch ev.Type {
			case streamItinerary:
				progress.Itinerary = true
			case streamPlaces:
				progress.Places = true
			case streamFailed:
				return
			}
		}
		if progress.Complete() {
			complete()
			return
		}
		if progress.GraceDue() && grace == nil {
			grace = time.After(s.streamPlacesGrace)
		}

		select {
		case <-ctx.Done():
			return
		case <-hard.C:
			fmt.Fprint(w, "event: timeout\ndata: {\"message\":\"Stream timeout\"}\n\n")
			flusher.Flush()
			return
		case <-heartbeat.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case <-grace:
			complete()
			return
		case <-bell:
		}
	}
}
