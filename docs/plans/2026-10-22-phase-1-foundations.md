# Phase 1 — Foundations Implementation Plan

**Goal:** Give the single binary the event-driven backbone the rest of the roadmap builds on: versioned
migrations, structured logs, a transport-agnostic event bus, a transactional outbox, idempotent consumers, a
trip event log that feeds SSE through Postgres LISTEN/NOTIFY, and the code split into per-service packages.

**Architecture:** `internal/bus` defines events, handlers and middleware with NATS and in-memory adapters. Services
(`internal/svc/{api,planner,discovery,router,agent}`) expose `Routes(mux)` and `Subscriptions()` and know nothing
about NATS. State changes that must produce an event write the event to an `outbox` table in the same transaction
(`internal/outbox`); a relay publishes after commit and a periodic sweep recovers anything missed. Consumers that
write to Postgres run through `internal/inbox`, which records processed event IDs so redelivery is harmless.
Everything a client sees on SSE is read from the `trip_events` table, woken by `LISTEN/NOTIFY`
(`internal/tripevents`), which also fixes the old subscribe-after-read race. `cmd/monolith` only wires services.

**Tech stack:** Go 1.25, pgx v5, goose v3 (migrations), `log/slog`, NATS JetStream, Postgres 16, Redis.

**Spec:** `docs/design/2026-10-22-trequila-refocus-design.md` (Roadmap → Phase 1; "Messaging, outbox, SSE").

## Global constraints

- Work on `main`; one Conventional Commit per task step that changes behaviour; **no AI attribution**.
- Every commit builds, vets and passes `go test ./...`. Integration tests need Postgres and are skipped unless
  `TEST_DATABASE_URL` is set (`make test-int` sets it from the compose database); CI always sets it.
- Not in this phase: grounded pipeline (Phase 2), Pub/Sub and OpenTelemetry (Phase 3), quotas (Phase 4), edits
  (Phase 5). The `trips` table keeps its JSONB result columns until Phase 2.
- NATS subjects stay `trips.*`. New durable names are `…-v4`; events without an `id` (the old envelope) are
  acknowledged and skipped.
- Keep: SSE `: keepalive` every 20s; the JWT_SECRET/ENCRYPTION_KEY startup checks; the `trips.places` ->
  `streamPlacesGrace` completion rule; plaintext keys only in memory.

## Review focus

1. Duplicate delivery of an itinerary/places/route event writes once and appends one `trip_event`.
2. A crash between commit and publish: the sweep publishes the pending outbox row; no event is lost, none doubled.
3. SSE: events appended while a client is between "replay" and "wait" are not missed; `Last-Event-ID` resumes
   without repeats; a failed trip ends with `failed` and no `complete`.
4. `Idempotency-Key` on `POST /api/trips`: same key + same body returns the original `request_id` and creates one
   trip; same key + different body is a 422.
5. Poison messages (bad JSON, legacy envelope, `bus.Permanent`) are acked and logged, never redelivered forever.

## Tasks

Each task: write the failing test, watch it fail, implement, watch it pass, run `go build ./... && go vet ./... &&
go test ./...`, commit.

### 1. Structured logging
- Create `internal/logging/logging.go`: `func Setup(level, format string) *slog.Logger` (`format` is `json` or
  `text`, default `text` on a TTY else `json`; `level` default `info`), installs it with `slog.SetDefault` so any
  remaining `log.Printf` is routed through it.
- Test: output format and level filtering with a buffer.
- Commit: `feat(logging): add slog setup driven by LOG_LEVEL and LOG_FORMAT`.

### 2. goose migrations and the migrate command
- Add `github.com/pressly/goose/v3`. Create `migrations/00001_baseline.sql` (users, trips, provider_keys,
  conversations as in `init.sql`, without dropped objects) and `migrations/00002_foundations.sql` (see Task 4-8
  tables). Embed with `migrations/embed.go` (`package migrations`, `//go:embed *.sql`, `var FS embed.FS`).
- `cmd/migrate/main.go`: `migrate up|down|status|version` using `DATABASE_URL`; `down` rolls back one step.
- Remove `init.sql` and the legacy `migrations/2026-*.sql`; compose gains a one-shot `migrate` service the `api`
  waits on (`condition: service_completed_successfully`); Dockerfile also builds `/app/migrate`; `Procfile` gets
  `release: ./bin/migrate up`.
- Test (integration): `up` then `down` to 0 then `up` again succeeds on an empty schema.
- Commit: `feat(db): manage the schema with goose migrations`.

### 3. Test database helper and make targets
- `internal/testdb/testdb.go`: `func New(t testing.TB) *sql.DB` — skips when `TEST_DATABASE_URL` is unset, creates
  a unique schema, sets `search_path`, applies `migrations.FS`, drops the schema on cleanup. Also `URL(t)`.
- `Makefile`: `test-int` runs `go test ./...` with `TEST_DATABASE_URL` pointing at the compose database.
- Commit: `test: add a Postgres test helper and a test-int target`.

### 4. Event bus core
- `internal/bus/bus.go`: `Event{ID, Type, TraceParent string; Data json.RawMessage}` with `NewEvent(typ, id string,
  payload any) (Event, error)` and `(Event).Decode(v any) error`; `Publisher`; `Handler func(context.Context,
  Event) error`; `Subscription{Durable, Type string; AckWait time.Duration; MaxDeliver int; Handler Handler}`;
  `Bus` (= `Publisher` + `Subscribe(ctx, Subscription) error`, blocks until ctx is done); `Pinger` (`Ping(ctx)
  error`); `Permanent(err) error`, `IsPermanent(err) bool`; `Middleware`, `Chain`, `Recover(*slog.Logger)`,
  `Logging(*slog.Logger, name)`.
- `internal/bus/memory`: in-process `Bus` with the same ack/nak/permanent semantics (nak redelivers up to
  `MaxDeliver`), for tests.
- Tests: decode round trip; panic recovered and treated as a retryable error; permanent errors are not
  redelivered; transient errors are redelivered up to `MaxDeliver`.
- Commit: `feat(bus): add the event bus contract, middleware and an in-memory adapter`.

### 5. NATS adapter
- `internal/bus/natsbus`: `Connect(ctx, Config{URL, CredsFile, CredsContent}) (*Bus, error)` ensuring the `TRIPS`
  stream (settings from today's `internal/nats`); `Publish` sets `Nats-Msg-Id` to `Event.ID`; `Subscribe` creates
  a durable pull consumer (explicit ack, the subscription's `AckWait`/`MaxDeliver`), decodes the envelope
  `{"id","type","traceparent","data"}`, skips envelopes without an `id`, acks on success and on `Permanent`, naks
  with a 2s delay otherwise; `Ping`.
- Test with an embedded `nats-server` (JetStream, temp dir): publish → handler; transient error → redelivery;
  permanent → single delivery; legacy envelope skipped.
- Commit: `feat(bus): add a NATS JetStream adapter`.

### 6. Outbox
- Migration table `outbox(id bigserial pk, event_id text unique, type text, data jsonb, traceparent text,
  created_at timestamptz, published_at timestamptz, attempts int, last_error text)` + partial index on
  `published_at IS NULL`.
- `internal/outbox`: `Add(ctx, tx *sql.Tx, e bus.Event) error`; `Relay{DB, Pub, Log}` with `Flush(ctx, limit)
  (int, error)` (select unpublished `FOR UPDATE SKIP LOCKED`, publish, mark published, record `attempts` and
  `last_error` on failure), `Kick(ctx)` (Flush under a 2s budget, logging errors) and `Run(ctx, every)`.
- Tests (integration): rows added in a rolled-back tx never publish; committed rows publish once; a failing
  publisher leaves the row pending with `attempts` incremented; two concurrent relays never publish a row twice.
- Commit: `feat(outbox): add a transactional outbox with a relay`.

### 7. Inbox (idempotent consumers)
- Migration table `processed_events(consumer text, event_id text, processed_at timestamptz, primary key
  (consumer, event_id))`.
- `internal/inbox`: `Handle(db *sql.DB, consumer string, fn func(ctx, *sql.Tx, bus.Event) error) bus.Handler` —
  begins a tx, `INSERT … ON CONFLICT DO NOTHING`; if nothing inserted, returns nil (duplicate); otherwise runs
  `fn` and commits; any `fn` error rolls back (including the marker) and is returned; `Purge(ctx, db, olderThan)`.
- Tests (integration): same event twice → `fn` runs once; `fn` error → marker rolled back and the retry runs `fn`.
- Commit: `feat(inbox): add idempotent event handling backed by processed_events`.

### 8. Trip event log and LISTEN/NOTIFY hub
- Migration: `trip_events(id bigserial pk, trip_id text references trips on delete cascade, type text, data jsonb,
  created_at)` + index `(trip_id, id)`; trigger function `pg_notify('trip_events', NEW.trip_id)` after insert.
- `internal/tripevents`: `Event{ID int64; Type string; Data json.RawMessage}`; `Append(ctx, tx, tripID, typ string,
  data any) (int64, error)`; `After(ctx, db, tripID string, afterID int64) ([]Event, error)`;
  `Hub` — `NewHub(connString string, log *slog.Logger)`, `Run(ctx)` (one dedicated pgx connection, LISTEN,
  reconnect with backoff, closes all doorbells on reconnect so waiters re-query), `Subscribe(tripID string)
  (<-chan struct{}, func())` (buffered(1) doorbell, never blocks the hub).
- Tests (integration): `After` ordering and filtering; an `Append` in a committed tx rings the doorbell, a rolled
  back one does not; two subscribers on different trips are independent.
- Commit: `feat(tripevents): add the trip event log and a LISTEN/NOTIFY hub`.

### 9. Trips store
- Migration: `trip_members(trip_id, user_id, role text, primary key (trip_id, user_id))`;
  `idempotency_keys(user_id text, key text, request_hash text, trip_id text, created_at, primary key (user_id, key))`.
- `internal/trips`: `Store{DB, Relay *outbox.Relay}`; `CreateParams{UserID, Source, Destination, StartDate,
  EndDate string; Days, Budget int; Interests []string; Provider, Model, IdempotencyKey string}`;
  `(*Store).Create(ctx, p) (requestID string, created bool, err error)`; sentinel `ErrIdempotencyConflict`.
  One tx inserts the trip (`status='pending'`), the owner row in `trip_members`, an outbox `trips.requested` event
  (ID `trips.requested:<requestID>`), and the idempotency record; after commit it calls `Relay.Kick`.
- Tests (integration): creation writes trip, member and a pending outbox row; same key + same params returns the
  same `requestID` with `created=false`; same key + different params returns `ErrIdempotencyConflict`.
- Commit: `feat(trips): create trips atomically with their outbox event`.

### 10. Service packages (additive)
Create the packages beside the old code; nothing is wired yet.
- `internal/events` — subject constants (`TripRequested="trips.requested"`, `TripItinerary`, `TripImages`,
  `TripPlaces`, `TripRouteRequested`, `TripRoute`, `TripFailed`) and payload types moved from `internal/types`
  where they are event-specific; helper `ID(typ, requestID string) string`.
- `internal/svc/planner`: `New(keys *providerkeys.Store, pub bus.Publisher, newProvider ProviderFactory)`,
  `Subscriptions()` (durable `planner-v4`, AckWait 2m, MaxDeliver 3). Handler resolves the key, builds the provider
  (`defer Close()`), generates, publishes `trips.itinerary`; auth failures publish `trips.failed` then return
  `bus.Permanent`; other errors are returned for retry. `ProviderFactory` defaults to `llm.NewProvider`.
- `internal/svc/discovery`: places handler (durable `places-v4`, AckWait 5m, MaxDeliver 3) and images handler
  (durable `images-v4`, AckWait 30s), same behaviour as today's workers.
- `internal/svc/router`: route handler (durable `route-v4`, AckWait 5m) using `internal/route`; event types
  `trips.route_requested` / `trips.route` move here from `internal/route` (which keeps only the pure logic).
- Tests with the memory bus and `httptest` upstreams: planner publishes an itinerary for a fake provider and a
  failure + permanent for an auth error; discovery publishes places/images; router publishes a route.
- Commits: `feat(svc): add the planner service on the event bus`, `… discovery …`, `… router …`.

### 11. API service (additive)
- `internal/svc/api`: moves the handlers from `cmd/monolith` (health/ping, auth, current user, keys, trips list/get/
  delete, replan, optimize) keeping their behaviour; `POST /api/trips` calls `trips.Store.Create` and honours the
  `Idempotency-Key` header (422 on conflict); `GET /api/trips/stream/{id}` serves SSE from `tripevents`
  (subscribe to the hub **before** replaying, `id:` is the event ID, `Last-Event-ID` header or `lastEventId` query
  resumes, `streamProgress`/`streamPlacesGrace`/heartbeat/hard timeout as today).
- Result consumers as `Subscriptions()` wrapped by `inbox.Handle` (durables `api-itinerary-v4`, `api-images-v4`,
  `api-places-v4`, `api-route-v4`, `api-failures-v4`, 30s AckWait): each updates `trips` and appends the matching
  `trip_event` in the same tx; the itinerary consumer also adds a `trips.route_requested` outbox event carrying the
  trip's stored places; the failures consumer sets `status='failed'` + `error_reason`. `POST …/optimize/{id}` adds the
  same outbox event.
- Move the existing `cmd/monolith` tests with their code.
- Tests: SSE integration (below), duplicate delivery of each result event writes once, idempotency-key behaviour
  through HTTP.
- Commit: `feat(svc): add the API service backed by outbox, inbox and trip events`.

### 12. Agent service
- `internal/svc/agent`: `New(...)` wires `internal/mcp` and `internal/chatbot`; `Routes` registers `/mcp` and
  `/api/chat` behind the auth middleware. `internal/mcp.NewServer` takes `*trips.Store` instead of `js`/`nc`;
  `handleCreateTrip` calls `trips.Store.Create` (removing the duplicated insert-then-publish).
- Commit: `refactor(mcp): create trips through the trips store`.

### 13. Switch the monolith to the services
- `internal/config` (`Load() Config`: ports, URLs, secrets, `LOG_LEVEL`, `LOG_FORMAT`, NATS creds, frontend URL).
- `cmd/monolith/main.go` becomes wiring only: logging, config, DB pool, `natsbus.Connect`, outbox relay
  (`Run` every 10s), tripevents hub, build the five services, register `Routes`, start every `Subscriptions()`
  with the standard middleware chain, HTTP server with CORS + rate limiting, graceful shutdown (cancel context,
  `srv.Shutdown`, wait for subscribers).
- Delete the replaced code: the old handlers/workers/consumers in `cmd/monolith`, `internal/planner` (loop),
  `internal/nats`, `route.StartWorker`/`PublishRequest`, and old event envelope types.
- End-to-end test `internal/svc/e2e_test.go` (integration; memory bus; `httptest` upstreams; fake LLM provider):
  register → store key → `POST /api/trips` → read `GET /api/trips/stream/{id}` → expect `images`, `places`,
  `itinerary`, `route`, then `complete`; assert one `trip_events` row per type and `status='completed'`.
  A second case injects an auth-failing provider and expects `failed` and no `complete`.
- Replace `log.*` in the remaining packages (`geo`, `images`, `cache`, `route`, `mcp`, `chatbot`, `auth`,
  `middleware`) with `slog` calls carrying attributes.
- Commits: `feat(config): load configuration in one place`, `refactor: run the services on the event bus`,
  `refactor: use structured logging across packages`.

### 14. CI
- `.github/workflows/ci.yml`: Postgres 16 service, `TEST_DATABASE_URL`, `go test -race ./...`, golangci-lint
  (`.golangci.yml`: `errcheck`, `govet`, `staticcheck`, `unused`, `ineffassign`, `gofmt`), a job that runs
  `migrate up`, `down` (to zero) and `up` against a fresh database, and the Docker build (builds both binaries).
- Commit: `ci: lint, race-test against Postgres and exercise migrations`.

### 15. Docs and decision records
- `docs/adr/001-service-boundaries-and-data-ownership.md`, `002-event-bus-and-delivery-semantics.md`,
  `003-transactional-outbox.md`, `004-sse-from-the-trip-event-log.md` (context, decision, consequences).
- Update `README.md` (architecture diagram), `CLAUDE.md` (architecture and conventions), `API_DOCUMENTATION.md`
  (SSE `id:`/`Last-Event-ID`, `Idempotency-Key`), `CHANGELOG.md`, `.env.example` (`LOG_LEVEL`, `LOG_FORMAT`),
  `CONTRIBUTING.md` (`make test-int`).
- Commit: `docs: record the Phase 1 architecture decisions and update the guides`.

### 16. Verify and publish
- `make check`, `make test-int`, `docker compose up --build` + the README quickstart (register → key → trip →
  SSE reaches `failed` with a fake key), `migrate down` then `up` on the compose database.
- Final whole-branch review; push `main`.
