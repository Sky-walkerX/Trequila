-- BYOK pivot migration (additive; safe to run on an existing database).
-- init.sql is the canonical schema for fresh installs; this brings older
-- deployments up to date without dropping data.

BEGIN;

-- Trips: provider/model snapshot + failure reason, and wider share codes.
ALTER TABLE trips ADD COLUMN IF NOT EXISTS provider     TEXT;
ALTER TABLE trips ADD COLUMN IF NOT EXISTS model        TEXT;
ALTER TABLE trips ADD COLUMN IF NOT EXISTS error_reason TEXT;
ALTER TABLE trips ALTER COLUMN share_code TYPE VARCHAR(32);

-- Per-user encrypted provider API keys (BYOK).
CREATE TABLE IF NOT EXISTS provider_keys (
    user_id        TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider       TEXT NOT NULL,
    encrypted_key  TEXT NOT NULL,
    base_url       TEXT,
    default_model  TEXT NOT NULL,
    is_default     BOOLEAN NOT NULL DEFAULT FALSE,
    created_at     TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    updated_at     TIMESTAMP WITH TIME ZONE DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (user_id, provider)
);

CREATE INDEX IF NOT EXISTS idx_provider_keys_user ON provider_keys(user_id);

COMMIT;
