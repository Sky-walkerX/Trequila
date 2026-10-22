-- +goose Up
-- Baseline schema. Every statement is idempotent so that `migrate up` can adopt a
-- database created by the old init.sql: the existing tables are left alone and the
-- legacy objects below are removed.

CREATE TABLE IF NOT EXISTS users (
    id TEXT PRIMARY KEY,
    email VARCHAR(255) UNIQUE NOT NULL,
    name VARCHAR(255),
    password_hash VARCHAR(255),                       -- NULL for OAuth-only users
    provider VARCHAR(50) DEFAULT 'local',             -- 'local' | 'google'
    provider_id VARCHAR(255),                         -- OAuth provider's user ID
    avatar_url TEXT,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_users_email ON users(email);
CREATE INDEX IF NOT EXISTS idx_users_provider ON users(provider, provider_id);

CREATE TABLE IF NOT EXISTS trips (
    request_id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    source VARCHAR(255) NOT NULL,
    destination VARCHAR(255) NOT NULL,
    start_date DATE NOT NULL,
    end_date DATE NOT NULL,
    budget INTEGER,
    status VARCHAR(50) DEFAULT 'pending',   -- pending | completed | failed
    provider TEXT,                          -- LLM provider snapshot: gemini | anthropic | openai_compatible
    model TEXT,                             -- model snapshot at request time
    error_reason TEXT,                      -- set when status = 'failed'
    itinerary JSONB,
    images JSONB,
    places JSONB,
    route JSONB,                            -- per-day stop ordering derived from the itinerary
    created_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_trips_user_id ON trips(user_id);

-- Per-user provider API keys (BYOK). encrypted_key holds base64(nonce || AES-256-GCM
-- ciphertext); plaintext keys are never stored.
CREATE TABLE IF NOT EXISTS provider_keys (
    user_id        TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider       TEXT NOT NULL,           -- 'gemini' | 'anthropic' | 'openai_compatible'
    encrypted_key  TEXT NOT NULL,
    base_url       TEXT,                    -- required for openai_compatible endpoints
    default_model  TEXT NOT NULL,
    is_default     BOOLEAN NOT NULL DEFAULT FALSE,
    created_at     TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    updated_at     TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, provider)
);
CREATE INDEX IF NOT EXISTS idx_provider_keys_user ON provider_keys(user_id);

CREATE TABLE IF NOT EXISTS conversations (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    trip_id     TEXT,
    title       TEXT DEFAULT 'New Conversation',
    messages    JSONB NOT NULL DEFAULT '[]',
    created_at  TIMESTAMPTZ DEFAULT NOW(),
    updated_at  TIMESTAMPTZ DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_conversations_user ON conversations(user_id);
CREATE INDEX IF NOT EXISTS idx_conversations_trip ON conversations(trip_id);

-- Objects removed from the product before this baseline; only present on databases
-- that predate it.
DROP INDEX IF EXISTS idx_trips_share_code;
ALTER TABLE trips DROP COLUMN IF EXISTS vibes;
ALTER TABLE trips DROP COLUMN IF EXISTS share_code;
DROP TABLE IF EXISTS trip_collaborators;

-- +goose Down
DROP TABLE IF EXISTS conversations;
DROP TABLE IF EXISTS provider_keys;
DROP TABLE IF EXISTS trips;
DROP TABLE IF EXISTS users;
