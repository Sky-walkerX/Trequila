-- +goose Up
-- The per-trip event log that the SSE stream replays and follows. Rows are appended
-- in the same transaction as the state change they describe; the trigger rings a
-- Postgres notification so streaming connections wake up without polling.
CREATE TABLE trip_events (
    id         BIGSERIAL PRIMARY KEY,
    trip_id    TEXT NOT NULL REFERENCES trips(request_id) ON DELETE CASCADE,
    type       TEXT NOT NULL,
    data       JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_trip_events_trip ON trip_events (trip_id, id);

-- +goose StatementBegin
CREATE FUNCTION notify_trip_event() RETURNS trigger AS $$
BEGIN
    PERFORM pg_notify('trip_events', NEW.trip_id);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER trip_events_notify
    AFTER INSERT ON trip_events
    FOR EACH ROW EXECUTE FUNCTION notify_trip_event();

-- +goose Down
DROP TRIGGER IF EXISTS trip_events_notify ON trip_events;
DROP FUNCTION IF EXISTS notify_trip_event();
DROP TABLE IF EXISTS trip_events;
