// Package tripevents is the per-trip event log behind the SSE stream.
//
// Everything a client sees while a trip is being built is a row in trip_events,
// appended in the same transaction as the change it describes. A stream replays the
// rows it has not seen and then waits on a Hub "doorbell" until Postgres reports a
// new row. Because the doorbell is registered before the replay query runs, an event
// that lands in between is never missed, and because the log is durable, a client
// can reconnect with Last-Event-ID and pick up exactly where it left off.
package tripevents

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// Event is one row of the log. Data is the exact JSON object sent to clients.
type Event struct {
	ID   int64
	Type string
	Data json.RawMessage
}

// Append writes an event inside tx and returns its ID. data is marshalled to JSON
// unless it is already a json.RawMessage.
func Append(ctx context.Context, tx *sql.Tx, tripID, typ string, data any) (int64, error) {
	raw, err := toJSON(data)
	if err != nil {
		return 0, fmt.Errorf("tripevents: marshal %s: %w", typ, err)
	}
	var id int64
	err = tx.QueryRowContext(ctx,
		`INSERT INTO trip_events (trip_id, type, data) VALUES ($1, $2, $3) RETURNING id`,
		tripID, typ, []byte(raw)).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("tripevents: append %s: %w", typ, err)
	}
	return id, nil
}

func toJSON(v any) (json.RawMessage, error) {
	if raw, ok := v.(json.RawMessage); ok {
		return raw, nil
	}
	return json.Marshal(v)
}

// Querier is satisfied by *sql.DB and *sql.Tx.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// After returns the trip's events with an ID greater than afterID, oldest first.
func After(ctx context.Context, q Querier, tripID string, afterID int64) ([]Event, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, type, data FROM trip_events WHERE trip_id = $1 AND id > $2 ORDER BY id`,
		tripID, afterID)
	if err != nil {
		return nil, fmt.Errorf("tripevents: query: %w", err)
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var e Event
		var data []byte
		if err := rows.Scan(&e.ID, &e.Type, &data); err != nil {
			return nil, err
		}
		e.Data = data
		out = append(out, e)
	}
	return out, rows.Err()
}
