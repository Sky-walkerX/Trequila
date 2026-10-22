// Package inbox makes event handlers that write to Postgres safe under redelivery.
//
// The bus delivers at least once, so the same event can arrive twice. Handle records
// each processed event ID in the same transaction as the handler's own writes: either
// the writes and the marker commit together, or neither does. A duplicate finds the
// marker and is acknowledged without running the handler again.
package inbox

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/sky-walkerx/trequila/internal/bus"
)

// Handle returns a bus.Handler that runs fn at most once per (consumer, event ID).
// fn receives the transaction to do its writes in and must not commit or roll it
// back. Any error from fn rolls the transaction back, marker included, and is
// returned unchanged so the bus can decide between retrying and dropping.
func Handle(db *sql.DB, consumer string, fn func(ctx context.Context, tx *sql.Tx, e bus.Event) error) bus.Handler {
	return func(ctx context.Context, e bus.Event) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("inbox %s: begin: %w", consumer, err)
		}
		defer tx.Rollback()

		// A concurrent delivery of the same event blocks here until the first
		// transaction ends, then sees the marker (or, if it rolled back, takes over).
		res, err := tx.ExecContext(ctx,
			`INSERT INTO processed_events (consumer, event_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
			consumer, e.ID)
		if err != nil {
			return fmt.Errorf("inbox %s: mark: %w", consumer, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil // already applied
		}

		if err := fn(ctx, tx, e); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("inbox %s: commit: %w", consumer, err)
		}
		return nil
	}
}

// Purge deletes markers older than olderThan. Keep olderThan well above the broker's
// maximum redelivery horizon.
func Purge(ctx context.Context, db *sql.DB, olderThan time.Duration) (int64, error) {
	res, err := db.ExecContext(ctx,
		`DELETE FROM processed_events WHERE processed_at < now() - make_interval(secs => $1)`,
		olderThan.Seconds())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
