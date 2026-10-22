-- +goose Up
-- Which events each consumer has already applied. Written in the same transaction as
-- the consumer's own changes, so a redelivered event is recognised and skipped.
CREATE TABLE processed_events (
    consumer     TEXT NOT NULL,
    event_id     TEXT NOT NULL,
    processed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (consumer, event_id)
);
CREATE INDEX idx_processed_events_at ON processed_events (processed_at);

-- +goose Down
DROP TABLE IF EXISTS processed_events;
