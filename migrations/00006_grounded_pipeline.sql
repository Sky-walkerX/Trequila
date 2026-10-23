-- +goose Up
-- Tables for the grounded pipeline. Each has one writing service (ADR 001): discovery
-- owns places, trip_candidates, trip_weather and trip_images; the planner owns
-- itinerary_versions and stage_runs; the router owns itinerary_routes; the API owns the
-- new trips columns. Additive only: the old JSONB result columns go in a later
-- migration, once nothing reads them.

ALTER TABLE trips
    ADD COLUMN timezone        TEXT,                       -- IANA zone of the destination
    ADD COLUMN dest_lat        DOUBLE PRECISION,
    ADD COLUMN dest_lon        DOUBLE PRECISION,
    ADD COLUMN interests       JSONB NOT NULL DEFAULT '[]',
    ADD COLUMN current_version INTEGER NOT NULL DEFAULT 0; -- 0 = no itinerary yet
-- trips.status now also takes: discovering | planning | routing | ready.

-- A cache of OpenStreetMap places, keyed by their OSM reference ("node/123").
CREATE TABLE places (
    osm_ref       TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    kind          TEXT NOT NULL CHECK (kind IN ('sight', 'meal')),
    category      TEXT NOT NULL,
    lat           DOUBLE PRECISION NOT NULL,
    lon           DOUBLE PRECISION NOT NULL,
    opening_hours TEXT,
    setting       TEXT NOT NULL DEFAULT 'unknown' CHECK (setting IN ('indoor', 'outdoor', 'unknown')),
    free_entry    BOOLEAN NOT NULL DEFAULT FALSE,
    wikidata      TEXT,
    wikipedia     TEXT,
    website       TEXT,
    fetched_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The short list a trip is planned from, with the reasons each place was chosen.
CREATE TABLE trip_candidates (
    trip_id  TEXT NOT NULL REFERENCES trips(request_id) ON DELETE CASCADE,
    osm_ref  TEXT NOT NULL REFERENCES places(osm_ref),
    rank     INTEGER NOT NULL,
    score    DOUBLE PRECISION NOT NULL,
    reasons  JSONB NOT NULL DEFAULT '[]',
    signals  JSONB NOT NULL DEFAULT '{}',
    PRIMARY KEY (trip_id, osm_ref)
);

CREATE TABLE trip_weather (
    trip_id   TEXT NOT NULL REFERENCES trips(request_id) ON DELETE CASCADE,
    day       DATE NOT NULL,
    code      INTEGER NOT NULL,
    tmax      DOUBLE PRECISION,
    tmin      DOUBLE PRECISION,
    rain_prob INTEGER NOT NULL DEFAULT 0,
    source    TEXT NOT NULL CHECK (source IN ('forecast', 'typical')),
    PRIMARY KEY (trip_id, day)
);

CREATE TABLE trip_images (
    trip_id     TEXT NOT NULL REFERENCES trips(request_id) ON DELETE CASCADE,
    position    INTEGER NOT NULL,              -- 0 is the hero image
    url         TEXT NOT NULL,
    thumb_url   TEXT,
    source      TEXT NOT NULL,
    attribution TEXT NOT NULL DEFAULT '',
    license     TEXT NOT NULL DEFAULT '',
    page_url    TEXT NOT NULL DEFAULT '',
    width       INTEGER,
    height      INTEGER,
    PRIMARY KEY (trip_id, position)
);

-- Every itinerary the trip has had. A new version never overwrites the last, so undo is
-- a pointer move. Items and days carry stable ids inside content.
CREATE TABLE itinerary_versions (
    trip_id    TEXT NOT NULL REFERENCES trips(request_id) ON DELETE CASCADE,
    version    INTEGER NOT NULL CHECK (version >= 1),
    parent     INTEGER,
    cause      TEXT NOT NULL,                  -- 'generated' | an edit operation
    content    JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (trip_id, version)
);

-- The scheduled, travel-timed version of one itinerary version.
CREATE TABLE itinerary_routes (
    trip_id    TEXT NOT NULL,
    version    INTEGER NOT NULL,
    content    JSONB NOT NULL,
    estimated  BOOLEAN NOT NULL DEFAULT FALSE, -- some travel times are straight-line estimates
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (trip_id, version),
    FOREIGN KEY (trip_id, version) REFERENCES itinerary_versions (trip_id, version) ON DELETE CASCADE
);

-- A lease on a unit of expensive work, so a redelivered event does not repeat it (for
-- example the paid LLM call). finished_at stays NULL while the work runs or crashed.
CREATE TABLE stage_runs (
    trip_id     TEXT NOT NULL REFERENCES trips(request_id) ON DELETE CASCADE,
    stage       TEXT NOT NULL,
    started_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ,
    PRIMARY KEY (trip_id, stage)
);

-- +goose Down
DROP TABLE IF EXISTS stage_runs;
DROP TABLE IF EXISTS itinerary_routes;
DROP TABLE IF EXISTS itinerary_versions;
DROP TABLE IF EXISTS trip_images;
DROP TABLE IF EXISTS trip_weather;
DROP TABLE IF EXISTS trip_candidates;
DROP TABLE IF EXISTS places;
ALTER TABLE trips
    DROP COLUMN IF EXISTS current_version,
    DROP COLUMN IF EXISTS interests,
    DROP COLUMN IF EXISTS dest_lon,
    DROP COLUMN IF EXISTS dest_lat,
    DROP COLUMN IF EXISTS timezone;
