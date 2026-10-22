-- +goose Up
-- Events that must be published exactly because a state change committed. Rows are
-- written in the same transaction as the change; the relay publishes them afterwards.
CREATE TABLE outbox (
    id           BIGSERIAL PRIMARY KEY,
    event_id     TEXT NOT NULL UNIQUE,
    type         TEXT NOT NULL,
    data         JSONB NOT NULL,
    traceparent  TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at TIMESTAMPTZ,
    attempts     INTEGER NOT NULL DEFAULT 0,
    last_error   TEXT
);
CREATE INDEX idx_outbox_pending ON outbox (id) WHERE published_at IS NULL;

-- +goose Down
DROP TABLE IF EXISTS outbox;
