-- migrations/2026-07-30-route.sql
--
-- Per-day stop ordering for a trip: which activities to visit in what order,
-- with straight-line distances and estimated durations between them.

ALTER TABLE trips ADD COLUMN IF NOT EXISTS route JSONB;
