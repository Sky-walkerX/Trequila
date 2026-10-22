-- Phase 0 of the refocus: vibes, share/join and collaborators are gone.
-- Idempotent. Run after the matching backend deploy (the old binary still selects vibes).
ALTER TABLE trips DROP COLUMN IF EXISTS vibes;
DROP INDEX IF EXISTS idx_trips_share_code;
ALTER TABLE trips DROP COLUMN IF EXISTS share_code;
DROP TABLE IF EXISTS trip_collaborators;
