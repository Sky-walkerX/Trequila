-- +goose Up
-- Who belongs to a trip. Every trip has its owner as a member today; other roles are
-- added when shared editing arrives.
CREATE TABLE trip_members (
    trip_id   TEXT NOT NULL REFERENCES trips(request_id) ON DELETE CASCADE,
    user_id   TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role      TEXT NOT NULL DEFAULT 'owner',
    joined_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (trip_id, user_id)
);
CREATE INDEX idx_trip_members_user ON trip_members (user_id);

-- Remembers the outcome of a request that carried an Idempotency-Key header, so a
-- retried POST returns the original result instead of creating a second trip.
CREATE TABLE idempotency_keys (
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    key          TEXT NOT NULL,
    request_hash TEXT NOT NULL,
    trip_id      TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, key)
);

-- +goose Down
DROP TABLE IF EXISTS idempotency_keys;
DROP TABLE IF EXISTS trip_members;
