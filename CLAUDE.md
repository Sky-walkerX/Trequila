# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Trequila is an open-source, **bring-your-own-key (BYOK)** AI trip-planning API in Go. A user stores their
own LLM API key (Gemini / Anthropic / any OpenAI-compatible endpoint), submits a trip request, and the
system asynchronously generates an itinerary, destination images, and nearby places, streaming results back
over Server-Sent Events. All enrichment (geocoding, POIs, images) is **keyless** — no operator API keys are
needed to self-host beyond a database, Redis, and NATS.

The project is being refocused into a grounded, editable trip planner on GCP; the approved design is
[docs/design/2026-10-22-trequila-refocus-design.md](docs/design/2026-10-22-trequila-refocus-design.md)
and it is delivered phase by phase (plans in `docs/plans/`). Phase 0 (cutting the features the refocus drops) is complete.

The design spec that drove the BYOK conversion is
[docs/design/2026-06-16-open-source-byok-pivot-design.md](docs/design/2026-06-16-open-source-byok-pivot-design.md);
the MCP/chatbot spec is [docs/design/2026-06-16-mcp-chatbot-design.md](docs/design/2026-06-16-mcp-chatbot-design.md).
Both are still the best reference for *why* things are shaped the way they are.

## Commands

```bash
go build -o monolith ./cmd/monolith   # build (Procfile/Dockerfile do the same)
go run ./cmd/monolith                 # needs env vars + external NATS, Postgres, Redis (.env.example)
go test ./...                         # full suite (fast, no external services required)
go test ./internal/crypto -run TestEncryptDecryptRoundTrip -v   # single test
go vet ./...
gofmt -l .

make env && make up                   # local stack: API + Postgres + NATS JetStream + Redis (docker compose)
make check                            # gofmt + go vet + go test (no services needed)
make test-int                         # the same plus the Postgres-backed tests (starts the db service)
golangci-lint run ./...               # the CI linter (config in .golangci.yml)
make help                             # logs, psql, down, reset, ...
psql "$DATABASE_URL" -f init.sql      # canonical schema; migrations/ holds the incremental deltas
```

`go.mod` declares **go 1.25.5**; the local toolchain lives at `~/sdk/go` (already on PATH in `~/.zshrc`,
currently go1.26.4). Docker fallback if the local toolchain is missing:
`docker run --rm -v "$PWD":/app -w /app -v trequila-gocache:/go/pkg/mod golang:1.25 go build ./...`.

**Testing is deliberately light** — tests exist only for tricky or security-critical pure logic
(`internal/crypto`, `internal/ai` JSON cleanup, `internal/providerkeys`, `internal/geo`/`internal/images`
HTTP parsing against `httptest` servers, `cmd/monolith` key handlers and SSE completion logic). Rely on `go build` / `go vet`
elsewhere rather than adding broad test scaffolding.

Schema changes are goose migrations in `migrations/` (`NNNNN_name.sql` with `-- +goose Up/Down`). The
`migrate` binary applies them (`make up` runs it as a one-shot service; Heroku runs it in the release phase).
Every migration needs a working `Down`: CI applies, resets and re-applies them.

**Tests.** `go test ./...` needs no services: tests that need Postgres skip unless `TEST_DATABASE_URL` is set
(`make test-int` sets it from the compose database; CI always sets it). Each such test gets its own throwaway
schema from `internal/testdb`. Tests cover tricky or security-critical logic and the event flows end to end
(`internal/svc/e2e_test.go` runs the real services on the in-memory bus); do not mock Postgres.

## Architecture: five services in one binary

`cmd/monolith` is wiring only. It builds five services under `internal/svc` — **api** (HTTP, accounts, trips,
keys, the progress stream; owns the `trips` data), **planner** (itinerary from the user's LLM key),
**discovery** (places and images), **router** (orders each day) and **agent** (`/mcp` and `/api/chat`) — and
runs them as goroutines. They never import each other. They communicate through `internal/bus`, a
transport-neutral event contract with a NATS JetStream adapter (`natsbus`, stream `TRIPS`, subjects `trips.*`)
and an in-memory one for tests. Each service exposes `Routes(mux)` and/or `Subscriptions()`. The reasoning is in
`docs/adr/` — read ADRs 001–004 before changing how services or events work.

Delivery is **at least once**. A handler returning an error is redelivered (up to `MaxDeliver`); wrap an error
with `bus.Permanent` when redelivery cannot fix it. Handlers must therefore be idempotent.

### Async trip flow (the core path)

1. `POST /api/trips` → `trips.Store.Create` (shared with the MCP `create_trip` tool): validates, resolves the
   caller's provider key, and in **one transaction** inserts the trip (`status='pending'`, `{provider, model}`
   snapshotted), the owner's `trip_members` row, an `idempotency_keys` row when `Idempotency-Key` is sent, and
   a `trips.requested` event in the **outbox**. After the commit the relay publishes it immediately; a 10-second
   sweep (`outbox.Relay.Run`) recovers anything a crash or broker outage left behind.
2. Three subscriptions react to `trips.requested`:
   - **planner** (`planner-v4`) → re-resolves the key → `LLMProvider.GenerateItinerary` → `trips.itinerary`.
     A missing key or an auth/billing error publishes `trips.failed` and returns `bus.Permanent`; other
     errors are retried.
   - **discovery** `places-v4` → Nominatim geocode + Overpass → `trips.places` (a destination that cannot be
     geocoded publishes empty places; all three lookups failing is retried).
   - **discovery** `images-v4` → Wikipedia + Openverse → `trips.images`.
3. The API consumes the results (`api-*-v4`, each wrapped by `inbox.Handle`): in one transaction it updates the
   `trips` column, **appends a `trip_events` row** (what the stream replays) and, for the itinerary, adds a
   `trips.route_requested` outbox event carrying the trip's places. Itinerary arrival sets
   `status='completed'`; `trips.failed` sets `status='failed'` + `error_reason`.
4. **router** (`route-v4`) holds **no database**: the request carries itinerary, destination and places; whatever
   is still unplaced is geocoded through `geo.Service` within a 3-minute budget. It orders each day (clock order
   when activities carry times, otherwise nearest-neighbour + 2-opt on haversine distance) and publishes
   `trips.route`. Distances are **straight-line × 1.25**, a deliberate trade to keep a routing provider off the
   planning path. `POST /api/trips/optimize/{id}` queues a fresh request with a new event ID.
5. `GET /api/trips/stream/{id}` serves `trip_events` as SSE (ADR 004): the handler registers a `tripevents.Hub`
   doorbell **before** replaying, sends each event with `id:` = the row ID, honours `Last-Event-ID`, and ends on
   `failed`, on itinerary + places (`streamProgress`), or `streamPlacesGrace` (30s) after the itinerary if
   places never arrive, with a 5-minute hard cap. A `: keepalive` comment goes out every 20s — **do not remove
   it**: Heroku's router kills a response that sends nothing for ~55s (`H15`), and the frontend must skip the
   empty-data frame it produces.

   **Completion is deliberately not gated on `trips.route`**; it often lands after the stream ends and
   `GET /api/trips/{id}` serves it then.
6. A background job in the API fails trips still `pending` after 10 minutes (an event that exhausted its
   deliveries leaves no failure behind), and purges old outbox and processed-event rows.

**Event IDs are deterministic** where one input yields one output (`events.ID(type, tripID)`), which is what
lets the broker's de-duplication window, the outbox's unique key and the inbox ledger agree. **Durable names are
version-suffixed** (`planner-v4`, …): bump the suffix whenever you change a subscription's type or config, or
JetStream keeps the old consumer state.

### BYOK: keys, providers, factory

- `internal/crypto` — AES-256-GCM cipher built from `ENCRYPTION_KEY` (32-byte base64). Fails closed at
  startup; losing the key makes every stored user key unreadable.
- `internal/providerkeys` — the `provider_keys` table (PK `(user_id, provider)`), storing
  `base64(nonce || ciphertext)`. `Resolve(ctx, userID, overrideProvider)` returns a ready
  `ai.ProviderConfig` (empty override → the user's `is_default` row) or `ErrNoKey`. Plaintext keys only
  ever live in memory; `MaskKey` is used for display.
- `internal/ai` is the **leaf package**: `LLMProvider` interface, `ProviderConfig`, `ChatTurn`/`ToolCall`
  types, and shared `CleanJSON` / `ParseItinerary` / `BuildItineraryPrompt` / `Retry` / `IsAuthError` /
  `IsRetryable`. Adapters live in `internal/ai/{gemini,anthropic,openai}`.
- `internal/llm.NewProvider(ctx, cfg)` is the factory — it lives in its own package purely to avoid an
  import cycle with `internal/ai`. **Always construct providers through it**, never by importing an adapter
  directly outside of `internal/llm`.
- Default models when the user leaves `default_model` blank: `gemini-2.5-flash`, `claude-sonnet-4-6`, `gpt-4o`.
- Every `LLMProvider` must be `Close()`d (`defer llmProvider.Close()`) — the planner, chatbot and the
  remaining handlers all do.

### MCP server and the agentic chatbot

- `internal/mcp` exposes Trequila's capabilities over the **Streamable HTTP** transport at `POST /mcp`,
  behind normal JWT auth. Tools are split across `tools_trips.go` (create/get/status/list/delete),
  `tools_discovery.go` (geocode/search_places/get_images — the same `geo`/`images` services the workers use)
  and `tools_keys.go` (list/set/remove API key), plus resources and a `plan_trip` prompt. Tools read the
  caller from the request context via `auth.GetUserIDFromContext`, so they are automatically per-user.
- `internal/mcp/tool_adapter.go` re-exposes the same tools as `[]ai.ToolDefinition` + `ExecuteMCPTool`, so
  the in-process chatbot uses one tool registry rather than a second copy.
- `internal/chatbot` serves `POST /api/chat`: resolve the user's provider → load/create a `conversations`
  row → run an SSE-streamed agentic loop (`runAgentLoop`, **max 10 iterations**) over `ChatWithTools` →
  persist the conversation. `chatExcludedTools` withholds `set_api_key`, `remove_api_key`, and `delete_trip`
  from the chat loop specifically to limit prompt-injection blast radius — they stay available on `/mcp`,
  which is driven by an explicit client. Keep that list in mind when adding destructive tools.

### Synchronous LLM features (bypass the bus)

The weather replan resolves the user's key and calls the provider inside the HTTP handler (to be replaced
by an async `weather_replan` edit in Phase 5):

- **Weather replan** (`POST /api/trips/replan/{id}`): Open-Meteo forecast finds rainy days (WMO code ≥ 51),
  the LLM swaps outdoor activities for indoor ones, merged back into the stored itinerary.

## Conventions and gotchas

- **Failure model:** on LLM error, check `ai.IsAuthError` **before** `ai.IsRetryable` — auth/credit failures
  must fail fast: publish `trips.failed` with a reason and return `bus.Permanent`; they surface as an SSE
  `failed` event. Errors that may clear up (an outage, a timeout) are returned plainly and retried.
- **AckWait must exceed the worst-case processing time** of the handler, or the broker redelivers
  mid-flight: 2 min for the planner, 5 min for places (three sequential Overpass queries), 5 min for route
  (bounded by its own 3-min geocode budget). The five `api-*` result consumers are 30s each.
- **Provider base URLs are an SSRF surface.** `base_url` on an OpenAI-compatible key is user-chosen and the
  server calls it. `netguard.ValidateBaseURL` rejects non-https and internal hosts on save, and
  `netguard.Transport` (used by the OpenAI adapter) refuses to *connect* to non-public addresses, which also
  covers DNS rebinding and redirects. Operators who run a model on their own network opt in with
  `ALLOW_PRIVATE_PROVIDER_URLS=true`. Never return an upstream's response or error text to the caller.
- **Logging** is `log/slog` (`internal/logging`, `LOG_LEVEL`, `LOG_FORMAT`). Log with attributes (`"trip_id", id`),
  never string-built messages, and never log keys or tokens.
- **The HTTP server sets no `WriteTimeout`** (`ReadHeaderTimeout` guards the handshake instead). WriteTimeout
  is absolute from the start of the response, so *any* value caps SSE duration; each streaming handler
  enforces its own deadline.
- **Keyless enrichment etiquette:** Nominatim is rate-limited to ~1 req/s (there is an explicit sleep in
  `internal/geo`), and every outbound call sends `HTTP_USER_AGENT`. All four upstreams
  (`OSM_NOMINATIM_URL`, `OSM_OVERPASS_URL`, `WIKIPEDIA_API_URL`, `OPENVERSE_API_URL`) are env-overridable so
  self-hosters can point at their own mirrors.
- **Overpass fails loudly, not emptily.** Its `[timeout:N]` budget must stay generous
  (`overpassQueryTimeout`, 50s) — at 15s dense cities returned **HTTP 504 with an HTML body**, which used to
  slip through as an empty result. `runOverpassQuery` checks the status code before decoding and retries
  once on 429/5xx. Never call `SearchPlaces` with the error discarded: a `nil` slice becomes a literal
  `null` in the `places` JSONB column, which then leaves the trip with no places at all.
- **Redis** (`internal/cache.NewRedisClient`) is a 7-day cache in front of the upstreams — a cache, not a
  source of truth. For `rediss://`, `ServerName` is the URL host **with the port stripped** (`opt.Addr` is
  `host:port` and never verifies), overridable via `REDIS_TLS_SERVER_NAME` for providers whose certificate
  names a different host than the URL does (Heroku Data for Redis). For providers with a private/self-signed
  CA (also Heroku Data for Redis), pin it via `REDIS_TLS_CA_CERT` rather than disabling verification.
  `REDIS_TLS_INSECURE_SKIP_VERIFY` exists as a last-resort, opt-in-only, loudly-logged escape hatch — do not
  make it the default path or remove its gate; prefer CA pinning whenever the provider's CA is obtainable.
- **Auth** (`internal/auth`): JWT (golang-jwt v5) guards every `/api/*` route and `/mcp`.
  `NewAuthService` **panics** if `JWT_SECRET` is unset or `< 32` bytes — this is intentional, do not soften
  it. Token is read from the `Authorization` header with an `auth_token` cookie fallback; Google OAuth uses
  an `oauth_state` HttpOnly cookie verified on callback. Post-callback, the JWT is delivered **both** ways:
  appended to the redirect as a URL fragment (`#token=...`, never a query param — fragments never reach the
  server, so this doesn't leak into access/proxy logs) *and* set as an HttpOnly cookie. The fragment is the
  primary channel the deployed SPA actually uses (read client-side into `localStorage`, sent back as an
  `Authorization` header) because the cookie is same-site-only (see the `SameSite=Lax` note below); the
  cookie is a same-site convenience, not the main path. There is **no** `/auth/token` dev endpoint anymore — do not re-add one.
- **CORS** (`middleware.DefaultCORSConfig`): exact-match allowlist from `CORS_ALLOWED_ORIGINS`
  (comma-separated), defaulting to `middleware.DefaultAllowedOrigin` =
  `https://trequila.namankhandelwal.dev`. `FRONTEND_URL` (same default) is where the OAuth callback
  redirects. Rate limiting is per-IP at 10 req/s burst 20, and `X-Forwarded-For`/`X-Real-IP` are honored
  **only** when `RemoteAddr` is a loopback/private/link-local address.
- **Config** (`internal/config`): `DATABASE_URL`, `REDIS_URL`, `NATS_URL`, `JWT_SECRET`, and
  `ENCRYPTION_KEY` are the real requirements; `JWT_SECRET` is validated in `auth.NewAuthService`, not in
  `config.Load`. There are **no operator LLM keys** — anything reintroducing one is a regression.
- **Deployment** targets Heroku today (`Procfile` runs `bin/migrate up` in the release phase, `app.json`,
  `heroku/go` buildpack); NATS credentials come from a file (`NATS_CREDS_FILE`, local) or inline content
  (`NATS_CREDS`). `ENABLE_KEEP_ALIVE=true` self-pings `/ping` every 25 min. The Dockerfile builds both
  `monolith` and `migrate`.
- **Observability**: Prometheus metrics (`internal/metrics`) are served at `/metrics` **only** when
  `METRICS_TOKEN` is set, and then behind that bearer token. `/ping` is a DB-free liveness check, `/health`
  checks DB + the bus. Bus deliveries, outbox backlog and trip counters are exported.
- **`API_DOCUMENTATION.md` is the authoritative HTTP reference** and matches `api.Service.Routes`. Update it
  in the same change whenever you add, remove, or reshape a route.
- **Commits must NOT include `Co-Authored-By: Claude` or any AI-attribution** (user preference).

## Remaining work / known gaps

The BYOK pivot (spec §13) is complete through step 11 and merged to `main`. Open items:

- The post-OAuth `auth_token` cookie is host-only with `SameSite=Lax`, so it only reaches the API on
  cross-origin XHR when the API and frontend share a registrable domain. If the API is deployed somewhere
  like `*.herokuapp.com`, the frontend must use the `Authorization` header.
- `/api/keys` returns `200` with an empty body on upsert/delete; a JSON body would be friendlier.
- **Before public release the owner must rotate the Gemini key that leaked on the old `origin/dev` branch.**
  The branch has been deleted and `main` rewritten, but deleting a branch does not revoke a key.
- **Local LLMs (Ollama / LM Studio) are deferred to v2** — a server cannot reach a user's localhost, so that
  path must be client-side. Don't add server-side local-LLM support without revisiting the spec.
