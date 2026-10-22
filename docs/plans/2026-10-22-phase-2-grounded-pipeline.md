# Phase 2 — Grounded Pipeline Implementation Plan

**Goal:** Make the itinerary trustworthy. It is built only from real places, respects opening hours, uses real
travel times and shows why each place was chosen. This is the product's core value.

**Architecture:** *Discovery* resolves the destination, fetches the weather and every nearby place in one
Overpass query, scores them deterministically (with human-readable reasons) and publishes a short list of
candidates. *Planner* asks the user's LLM to arrange **only those candidates** (by ID), validates the answer,
repairs it once, and falls back to a deterministic fill, so the stored plan is always grounded whatever the model
does. *Router* orders and schedules each day with real walking/driving times and flags any visit that falls
outside a place's opening hours. Each service owns its tables and publishes through the outbox. The API turns
the resulting events into the trip's status and the progress stream.

**Tech stack:** Go 1.25, Postgres, Redis, OpenStreetMap Overpass, Wikidata, Wikimedia (pageviews, Commons),
Open-Meteo, Photon, OpenRouteService (optional key), Pexels (optional key), `google/uuid` v7 for stable IDs.

**Spec:** `docs/design/2026-10-22-trequila-refocus-design.md` (Grounded pipeline, Data sources, Data model).
ADRs 001–004 still apply; this phase adds 005 (grounded planning) and 006 (hours-aware routing).

## Global constraints

- Work on `main`; Conventional Commits; no AI attribution; every commit builds, vets, lints and passes
  `go test -race ./...` (Postgres-backed tests run with `TEST_DATABASE_URL`).
- **Keyless by default.** Every operator key is optional: `ORS_API_KEY` (travel times; without it distances are
  haversine × 1.25 and legs are labelled `estimated`) and `PEXELS_API_KEY` (hero image; without it Wikimedia
  Commons). A self-hoster with no keys still gets a working, grounded plan.
- **Honesty over guessing.** Opening hours that cannot be parsed are `unknown`, never `closed`. Anything estimated
  is labelled so in the API.
- Every outbound call sends `HTTP_USER_AGENT`, honours the upstream's etiquette (Nominatim 1 req/s, an Overpass
  concurrency limit) and is cached. Upstream URLs stay env-overridable.
- One writer per table (ADR 001): discovery owns `places`, `trip_candidates`, `trip_weather`, `trip_images`;
  planner owns `itinerary_versions`, `stage_runs`; router owns `itinerary_routes`; api owns `trips`,
  `trip_events`. Events carry what the next service needs, so no service reads another's table.
- The frontend is intentionally not touched; the API shape may change freely this phase.
- The old JSONB result columns on `trips` are removed (a fresh start was decided; no data migration).

## Review focus

1. A model that returns unknown place IDs, duplicates, too few items, or non-JSON never produces an ungrounded
   plan: invalid items are repaired once, then replaced deterministically.
2. A place closed on the day it is scheduled is never silently scheduled: it is moved, or flagged as a conflict.
   Unparseable hours are `unknown` and are not treated as closed.
3. Every upstream can fail or be rate-limited: the trip still completes with what exists (no places at all fails
   the trip with a clear reason; no weather, no images and no ORS key do not).
4. A redelivered `trips.candidates_ready` does not call the LLM twice (stage lease) and does not create two
   itinerary versions.
5. Time zones: opening hours are evaluated in the destination's local time, including across a DST change.
6. Candidate selection is deterministic: the same inputs give the same list and the same reasons.

## Tasks

TDD throughout: write the failing test, watch it fail, implement, run `go build ./... && go vet ./... &&
golangci-lint run ./... && go test -race ./...`, commit.

### 1. Opening hours (`internal/hours`)
- `Parse(s string) (Schedule, error)` for a documented subset of the OSM `opening_hours` grammar: `24/7`; day
  ranges and lists (`Mo-Fr`, `Sa,Su`, wrap-around `Fr-Mo`); one or more time ranges (`09:00-12:00,14:00-18:00`);
  overnight ranges (`18:00-02:00`); `off`/`closed`; `;`-separated rules where later rules override earlier ones for
  the days they name; `PH` (public holiday) clauses are accepted and ignored. Anything else (sunrise/sunset, week
  numbers, months, `+`, comments) returns `ErrUnsupported`.
- `(Schedule).StateAt(t time.Time) State` (`Open`, `Closed`, `Unknown`), `(Schedule).Windows(d time.Time)
  []Window` (open intervals on that local date), `(Schedule).OpenOn(d time.Time) bool`.
- `Evaluate(raw string, t time.Time) State` convenience: empty or unsupported hours → `Unknown`.
- Tests (table-driven): the grammar above, overnight spill into the next day, rule override, `00:00-24:00`, a DST
  transition day, garbage input, and a corpus of ~40 real OSM strings with their expected state.
- Commit: `feat(hours): evaluate OSM opening_hours`.

### 2. OSM places client (`internal/osm`)
- One combined Overpass query around a point (sights: `tourism` attraction/museum/gallery/viewpoint/artwork/zoo/
  theme_park, `historic`, `leisure` park/garden/nature_reserve, `natural` beach/peak; food: `amenity`
  restaurant/cafe/bar/pub/fast_food, named only; no accommodation) returning `Element{Type, ID, Name, Lat, Lon,
  Tags}` (centre for ways and relations).
- `Client.Around(ctx, lat, lon, radiusM)`: retries once on 429/5xx, fails over between configured mirrors
  (`OVERPASS_URLS`), a process-wide concurrency limit (default 2), a 30-day Redis cache keyed by rounded
  coordinates and radius. A non-JSON body (Overpass returns HTML on overload) is an error, never an empty result.
- `Element` helpers: `Kind()` (`sight`/`meal`), `Category()`, `OpeningHours()`, `Wikidata()`, `Wikipedia()`,
  `Website()`, `Indoor()`, `Free()`.
- Tests with `httptest`: parsing a recorded response, HTML-on-504, mirror failover, retry, cache hit, the
  concurrency cap.
- Commit: `feat(osm): fetch nearby places with one combined Overpass query`.

### 3. Popularity (`internal/popularity`)
- `Client.Score(ctx, []string /*wikidata ids*/) (map[string]Signals, error)`, `Signals{Sitelinks, Pageviews int}`:
  Wikidata `wbgetentities` (props=sitelinks, ≤50 ids per call) and Wikimedia monthly pageviews for the English
  sitelink title (12 months); cached 30 days; a failure for one item degrades to zero signals and never fails the
  batch.
- Tests with `httptest`: batching over 50, missing entities, pageview 404, cache.
- Commit: `feat(popularity): rank places by Wikidata sitelinks and Wikipedia pageviews`.

### 4. Weather and geocoding (`internal/weather`)
- `Client.Forecast(ctx, lat, lon, tz string, from, to time.Time) ([]Day, error)`; `Day{Date, Code, TMax, TMin,
  RainProb, Source}`. Within 16 days of today: Open-Meteo forecast; otherwise the same dates a year earlier from the
  archive API with `Source:"typical"`. `Day.Wet()` is true for WMO codes ≥ 51 or rain probability ≥ 60.
- `Geocode(ctx, name) (Destination{Name, Country, Lat, Lon, Timezone}, error)` using Open-Meteo geocoding, falling
  back to Nominatim (through `internal/geo`'s rate limit) when it finds nothing. Not found → `ErrNotFound`.
- Tests with `httptest`: forecast vs archive selection by date, wet detection, geocode fallback, not found.
- Commit: `feat(weather): geocode destinations and fetch forecast or typical weather`.

### 5. Scoring (`internal/scoring`)
- `Rank(in Input) Result`, `Input{Places []Candidate; Interests []string; Days []weather.Day; Center Point; TripDays
  int; Pace string}`. Per place: interest match, popularity (log-scaled sitelinks + pageviews), distance decay from
  the centre, open-on-trip-days (closed on every trip day → excluded; unknown → a small penalty), an indoor bonus on
  wet days, fee note. Every contribution adds a reason string. Selection: the top `days×sights` sights and
  `days×meals` meals by score, with per-category caps and de-duplication (same name within 150 m). A pure function.
- Tests: golden ranking for a fixture city; determinism (shuffled input → identical output); closed-all-days
  excluded; unknown hours kept; wet day prefers indoor; category caps; reasons present and stable.
- Commit: `feat(scoring): rank candidates deterministically with reasons`.

### 6. Schema (`migrations/00006_grounded_pipeline.sql`)
- Create `places`, `trip_candidates`, `trip_weather`, `trip_images`, `itinerary_versions`, `itinerary_routes`,
  `stage_runs`; add `trips.timezone`, `dest_lat`, `dest_lon`, `pace`, `interests`, `current_version`; statuses
  `pending → discovering → planning → routing → ready | failed`; drop the `itinerary`, `images`, `places`,
  `route` JSONB columns. A working `Down` restores the columns.
- Test: migrate up/down/up; uniqueness constraints.
- Commit: `feat(db): add the grounded-pipeline tables and drop the result blobs`.

### 7. Images (`internal/imagery`)
- `Hero(ctx, city)`: Pexels search (landscape, ≥1600px) when `PEXELS_API_KEY` is set, else Wikimedia Commons via the
  destination's Wikidata `P18`. `Gallery(ctx, city, n)`: Commons search filtered to landscape, ≥1200px, raster, an
  allowed licence (CC0, CC BY, CC BY-SA, public domain), and no map/logo/flag titles. `PlacePhoto(wikidataID)` from
  `P18`. Every image carries `Source`, `Attribution`, `License`, `PageURL`. Failures never fail a trip.
- Tests with `httptest`: filtering rules, attribution fields, keyless fallback.
- Commit: `feat(imagery): choose destination images with attribution`.

### 8. Discovery service v2 (`internal/svc/discovery`)
- Subscribes to `trips.requested` (`discovery-v5`, ack wait 10 min). Steps: geocode (+timezone) → weather → OSM
  places (upsert `places`) → popularity → score and select (write `trip_candidates`, `trip_weather`) → images
  (best effort, write `trip_images`) → one transaction: inbox marker plus outbox events `trips.discovered` (destination
  facts, weather, selected candidates, images; the API stores it for the stream) and `trips.candidates_ready`
  (everything the planner needs, compact). Too few places → widen the radius once → otherwise `trips.failed` with a
  clear reason (`bus.Permanent`).
- Tests (memory bus, real Postgres, `httptest` upstreams): happy path writes the tables and events; an
  ungeocodable destination fails the trip; no places fails; weather/images down still succeeds; redelivery changes
  nothing.
- Commit: `feat(discovery): build ranked candidates from real places`.

### 9. Grounded planning (`internal/grounding`, `internal/svc/planner`)
- `grounding.Prompt` lists the candidates as a table (`id`, name, kind, hours summary, indoor, reasons) and demands
  JSON using only those IDs. `Validate(plan, candidates)` returns the problems (unknown ID, duplicate across days,
  wrong item count, kind mismatch). One repair re-prompt names the problems. `Fallback(candidates, days)` fills a
  plan deterministically (nearest-neighbour clustering per day, meals at lunch and dinner). The result is always
  valid. Items get stable UUIDv7 `item_id`s and days `day_id`s.
- Planner subscribes to `trips.candidates_ready` (`planner-v5`). A `stage_runs` lease (`INSERT … ON CONFLICT DO
  NOTHING`) stops a redelivery from calling the LLM again; the version row and the `trips.itinerary_version` outbox
  event commit together. Auth errors still publish `trips.failed` (permanent).
- Tests: a fake LLM returning (a) a perfect plan, (b) unknown IDs then a fixed plan, (c) unknown IDs twice
  (fallback), (d) prose instead of JSON, (e) too few days; the stored plan is always valid; redelivery makes one call.
- Commit: `feat(planner): plan only from verified candidates, validate and repair`.

### 10. Hours-aware routing (`internal/ors`, `internal/svc/router`)
- `ors.Client{Key}`: `Matrix(ctx, profile, coords)` and `Directions(ctx, profile, coords)`; Redis-cached; quota and
  transport errors fall back to haversine × 1.25 with `Estimated: true`. Walking unless a day's legs exceed 2 km,
  then driving.
- Router subscribes to `trips.itinerary_version` (`router-v5`), drops versions older than the trip's current one,
  orders each day (nearest-neighbour + 2-opt on travel time, honouring `pinned` items), simulates the day from
  09:00, places meals in their windows, and checks each visit with `hours.StateAt` in the destination's time zone:
  moves a closed place to a legal slot when one exists, otherwise records a conflict (`closed_at_arrival`,
  `closes_before_end`). Writes `itinerary_routes` and publishes `trips.route`.
- Tests: ordering golden; closed-at-arrival shifted to opening time; no legal slot → conflict; unknown hours never
  conflict; estimated legs labelled; ORS down → estimated fallback; a DST day.
- Commit: `feat(router): schedule days around opening hours with real travel times`.

### 11. API
- Consumers for the new events drive the status machine (`discovering → planning → routing → ready`) and append
  `trip_events`: `discovery` (destination, weather, candidates, images), `itinerary`, `route`, `failed`.
  `GET /api/trips/{id}` returns the new model (trip, weather, images, candidates with reasons, the current itinerary
  version, the route) and `GET /api/trips/{id}/versions[/{v}]`. The stream completes on `ready`, or `streamRouteGrace`
  after the itinerary if the route never arrives. `GET /api/geocode/autocomplete?q=` proxies Photon (cached, per-user
  rate limited). The reaper uses the new statuses.
- Tests: status machine, stream events and completion rules, GET shapes, autocomplete caching and limits.
- Commit: `feat(api): expose the grounded trip model and its progress`.

### 12. End to end, evaluation and docs
- Rewrite `internal/svc/e2e_test.go` for the new flow (fake LLM and upstreams): request → ready, with hours
  conflicts surfaced, and a model that returns garbage still yields a ready, valid plan.
- `cmd/evalplan`: runs the pipeline for a list of cities against the live upstreams and a chosen provider, prints
  the invalid-ID rate before and after repair, hours violations, km per day and time to ready, and writes
  `docs/eval/<date>.md`. Runs with `--llm=fallback` (no key) to evaluate everything except the model.
- ADRs 005 (grounded planning) and 006 (hours-aware routing); update README, CLAUDE.md, API_DOCUMENTATION.md,
  CHANGELOG and `.env.example` (`ORS_API_KEY`, `PEXELS_API_KEY`, `OVERPASS_URLS`).
- Commits: `test(e2e): cover the grounded flow`, `feat(evalplan): measure plan quality against live data`,
  `docs: record the grounding and routing decisions`.
