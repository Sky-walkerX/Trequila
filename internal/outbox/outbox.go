// Package outbox implements the transactional outbox.
//
// A state change that must emit an event writes the event with Add inside the same
// database transaction, so either both happen or neither does. A Relay publishes
// committed rows afterwards: Kick right after the commit for low latency, and Run on
// a timer as the safety net that recovers anything a crash or a broker outage left
// behind. Delivery is therefore at least once; consumers de-duplicate on event ID.
package outbox

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/sky-walkerx/trequila/internal/bus"
	"github.com/sky-walkerx/trequila/internal/metrics"
)

// Add records e for publication. Adding an event ID that is already present is a
// no-op, which makes retried transactions safe.
func Add(ctx context.Context, tx *sql.Tx, e bus.Event) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO outbox (event_id, type, data, traceparent)
		 VALUES ($1, $2, $3, NULLIF($4, ''))
		 ON CONFLICT (event_id) DO NOTHING`,
		e.ID, e.Type, []byte(e.Data), e.TraceParent)
	if err != nil {
		return fmt.Errorf("outbox: add %s: %w", e.Type, err)
	}
	return nil
}

// MaxAttempts is how many times the relay tries an event before parking it. A parked
// row stays in the table (with its last error) for an operator to inspect, but no longer
// holds up the events behind it.
const MaxAttempts = 5

// Relay publishes pending outbox rows.
type Relay struct {
	DB  *sql.DB
	Pub bus.Publisher
	Log *slog.Logger
}

func (r *Relay) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

// Flush publishes up to limit pending events in insertion order and returns how many
// went out. It stops at the first failure, recording the error on that row, so one
// bad event cannot be skipped silently; after MaxAttempts failures the row is parked
// and no longer selected. Rows locked by another relay are skipped, so any number of
// relays can run at once.
func (r *Relay) Flush(ctx context.Context, limit int) (int, error) {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx,
		`SELECT id, attempts, event_id, type, data, COALESCE(traceparent, '')
		 FROM outbox WHERE published_at IS NULL AND attempts < $2
		 ORDER BY id LIMIT $1 FOR UPDATE SKIP LOCKED`, limit, MaxAttempts)
	if err != nil {
		return 0, err
	}
	type pending struct {
		id       int64
		attempts int
		event    bus.Event
	}
	var batch []pending
	for rows.Next() {
		var p pending
		var data []byte
		if err := rows.Scan(&p.id, &p.attempts, &p.event.ID, &p.event.Type, &data, &p.event.TraceParent); err != nil {
			rows.Close()
			return 0, err
		}
		p.event.Data = data
		batch = append(batch, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	sent := 0
	var publishErr error
	for _, p := range batch {
		if err := r.Pub.Publish(ctx, p.event); err != nil {
			publishErr = fmt.Errorf("outbox: publish %s %s: %w", p.event.Type, p.event.ID, err)
			if p.attempts+1 >= MaxAttempts {
				r.log().Error("outbox event parked after repeated failures", "event_id", p.event.ID, "type", p.event.Type, "error", err)
			}
			if _, uerr := tx.ExecContext(ctx,
				`UPDATE outbox SET attempts = attempts + 1, last_error = $2 WHERE id = $1`, p.id, err.Error()); uerr != nil {
				return sent, uerr
			}
			break
		}
		if _, err := tx.ExecContext(ctx, `UPDATE outbox SET published_at = now() WHERE id = $1`, p.id); err != nil {
			return sent, err
		}
		sent++
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}

	var pendingCount int
	if err := r.DB.QueryRowContext(ctx, `SELECT count(*) FROM outbox WHERE published_at IS NULL AND attempts < $1`, MaxAttempts).Scan(&pendingCount); err == nil {
		metrics.OutboxPending.Set(float64(pendingCount))
	}
	return sent, publishErr
}

// Kick flushes right after a commit, under a short budget. A failure is logged and
// left for the next sweep; the caller's request must not fail because the broker is
// briefly unreachable.
func (r *Relay) Kick(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if _, err := r.Flush(ctx, 50); err != nil {
		r.log().Warn("outbox kick failed; the sweep will retry", "error", err)
	}
}

// Run sweeps the outbox every interval until ctx is cancelled, draining whatever it
// finds, and purges old published rows once an hour.
func (r *Relay) Run(ctx context.Context, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	lastPurge := time.Now()
	for {
		for {
			n, err := r.Flush(ctx, 100)
			if err != nil {
				if ctx.Err() == nil {
					r.log().Warn("outbox sweep failed", "error", err)
				}
				break
			}
			if n == 0 {
				break
			}
		}
		if time.Since(lastPurge) > time.Hour {
			if n, err := r.Purge(ctx, 7*24*time.Hour); err != nil {
				r.log().Warn("outbox purge failed", "error", err)
			} else if n > 0 {
				r.log().Info("outbox purged", "rows", n)
			}
			lastPurge = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Purge deletes published rows older than olderThan and returns how many it removed.
func (r *Relay) Purge(ctx context.Context, olderThan time.Duration) (int64, error) {
	res, err := r.DB.ExecContext(ctx,
		`DELETE FROM outbox WHERE published_at IS NOT NULL AND published_at < now() - make_interval(secs => $1)`,
		olderThan.Seconds())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
