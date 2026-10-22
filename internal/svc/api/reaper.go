package api

import (
	"context"
	"time"
)

const (
	reapInterval   = time.Minute
	stuckAfter     = 10 * time.Minute
	stuckReason    = "The trip took too long to plan. Please try again."
	reapBatchLimit = 100
)

// reapLoop fails trips that never received an itinerary. A planner that crashed, or
// retried its last time and gave up, leaves no failure event behind; without this the
// client would watch a pending trip forever.
func (s *Service) reapLoop(ctx context.Context) {
	t := time.NewTicker(reapInterval)
	defer t.Stop()
	for {
		if n, err := s.ReapStuck(ctx, stuckAfter); err != nil {
			if ctx.Err() == nil {
				s.log.Warn("reaping stuck trips failed", "error", err)
			}
		} else if n > 0 {
			s.log.Warn("failed trips that never got an itinerary", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ReapStuck marks pending trips older than olderThan as failed and appends the failure
// event their streams end on. It returns how many it failed.
func (s *Service) ReapStuck(ctx context.Context, olderThan time.Duration) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx,
		`SELECT request_id FROM trips
		 WHERE status = 'pending' AND created_at < now() - make_interval(secs => $1)
		 ORDER BY created_at LIMIT $2 FOR UPDATE SKIP LOCKED`, olderThan.Seconds(), reapBatchLimit)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, id := range ids {
		if err := s.markFailed(ctx, tx, id, stuckReason); err != nil {
			return 0, err
		}
	}
	return len(ids), tx.Commit()
}
