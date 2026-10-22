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
make check                            # gofmt + go vet + go test: what CI runs
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

`init.sql` is the source of truth for schema and is **not** applied automatically at startup;
`migrations/` holds the incremental deltas for existing databases (run them after deploying the matching
binary).

## Architecture: monolith of in-process "microservices"

Everything runs in **one binary** (`cmd/monolith/main.go`, ~1,970 lines — all HTTP handlers plus worker
bootstrap live there). The HTTP server and the workers run as goroutines and communicate **only through an
external NATS JetStream stream named `TRIPS`** (subjects `trips.*`, file storage, 7-day retention, created
idempotently in `internal/nats`). There is no in-process call between the API and the workers — the message
bus is the integration point, so the same code could be split into separate deployables unchanged.

### Async trip flow (the core path)

1. `POST /api/trips` resolves the caller's provider key (`providerkeys.Store.Resolve`), **snapshots
   `{provider, model}` onto the `trips` row**, inserts with `status='pending'`, and publishes
   `trips.requested` (deduplicated by `request_id` via `WithMsgID`).
2. Three workers fan out on `trips.requested`:
   - **Planner** (`internal/planner`) → re-resolves the user's key at processing time → `LLMProvider.GenerateItinerary` → publishes `trips.itinerary`.
   - **Imager** (`internal/images`) → Wikipedia REST summary (hero) + Openverse (gallery) → `trips.images`.
   - **Places** (`internal/geo`) → OSM Nominatim geocode + Overpass POIs → `trips.places`.
3. **Five consumer goroutines** in `main.go` (`consumeItineraries`/`consumeImages`/`consumePlaces`/
   `consumeRoutes`/`consumeFailures`) subscribe to those result subjects and persist each into
   the matching JSONB column of `trips`. Itinerary arrival flips `status='completed'`; `trips.failed` sets
   `status='failed'` + `error_reason`.
4. **Route** (`internal/route`) triggers on `trips.route_requested`, which `consumeItineraries` publishes
   once the itinerary is stored, and which `POST /api/trips/optimize/{id}` republishes on demand. The worker
   holds **no database**: the request carries the itinerary, destination and the trip's already-found places,
   and anything still unplaced is geocoded through the shared `geo.Service` (Redis-cached, ~1 lookup/second).
   It orders each day — clock order when the activities carry times, otherwise nearest-neighbour + 2-opt on
   haversine distance — and publishes `trips.route`. Distances are **straight-line × 1.25**, not street
   routing; that is a deliberate trade to keep a routing provider off the trip-planning path.
5. Client reads progress via SSE at `GET /api/trips/stream/{id}`: emit whatever is already in the DB, then
   live-subscribe to `trips.>` and forward `images`/`places`/`itinerary`/`route`/`failed` events until the
   itinerary and the places have both arrived (`streamProgress` in `cmd/monolith/stream_progress.go`;
   `streamPlacesGrace` 30s after the itinerary if places never arrive, `streamHardTimeout` 5-min cap).
   A `: keepalive` comment goes out every `streamHeartbeatInterval` (20s) — **do not remove it**: Heroku's
   router kills a response that sends nothing for ~55s with `H15 desc="Idle connection"`, and stages are
   routinely more than a minute apart.

   **Completion is deliberately not gated on `trips.route`.** Routing only starts once the itinerary is
   stored and then geocodes at about one lookup a second, so it frequently lands after the stream has
   finished; `GET /api/trips/{id}` serves it in that case. Note that a `: keepalive` comment reaches
   `@microsoft/fetch-event-source`'s `onmessage` with an **empty data buffer** — the frontend must skip
   those rather than parse them.

   The places worker Naks (JetStream redelivers) when an Overpass lookup fails and publishes `trips.places`
   only on success; it never publishes `trips.failed`. If places never arrive, the stream still completes
   with partial data once `streamPlacesGrace` expires after the itinerary.

**Producers vs. consumers:** producer workers generate and publish; consumer goroutines only persist. Each
is a durable JetStream consumer with a distinct, **version-suffixed `Durable` name**
(`planner-durable-v3`, `imager-worker-v1`, `places-worker-v2`, `route-worker-v1`,
`api-itinerary-writer-v1`, `api-route-writer-v1`, `api-failures-writer-v1`, …). **Bump the version suffix**
whenever you change a consumer's `FilterSubject` or config — JetStream will not otherwise reset existing
consumer state.

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

### Synchronous LLM features (bypass NATS)

The weather replan resolves the user's key and calls the provider inside the HTTP handler (to be replaced
by an async `weather_replan` edit in Phase 5):

- **Weather replan** (`POST /api/trips/replan/{id}`): Open-Meteo forecast finds rainy days (WMO code ≥ 51),
  the LLM swaps outdoor activities for indoor ones, merged back into the stored itinerary.

## Conventions and gotchas

- **Failure model:** on LLM error, check `ai.IsAuthError` **before** `ai.IsRetryable` — auth/credit failures
  must fail fast (no retry), publish `trips.failed` with a reason, and surface as an SSE `failed` event.
- **AckWait must exceed the worst-case processing time** of the handler, or JetStream redelivers
  mid-flight: 2 min for the planner, 5 min for places (three sequential Overpass queries), 5 min for route
  (bounded by its own 3-min geocode budget). The five `api-*-writer` consumers are 30s each.
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
- **Config** (`loadConfig` in `main.go`): `DATABASE_URL`, `REDIS_URL`, `NATS_URL`, `JWT_SECRET`, and
  `ENCRYPTION_KEY` are the real requirements; `JWT_SECRET` is validated in `auth.NewAuthService`, not in
  `loadConfig`. There are **no operator LLM keys** — anything reintroducing one is a regression.
- **Deployment** targets Heroku (`Procfile`, `app.json`, `heroku/go` buildpack); NATS credentials come from
  a file (`NATS_CREDS_FILE`, local) or inline content (`NATS_CREDS`, Heroku). `ENABLE_KEEP_ALIVE=true`
  self-pings `/ping` every 25 min.
- **Observability**: Prometheus at `/metrics` (`internal/metrics`); `/ping` is a DB-free liveness check,
  `/health` checks DB + NATS. `TripsCompletedTotal` and `TripsFailedTotal` are wired in the consumers.
- **`API_DOCUMENTATION.md` is the authoritative HTTP reference** and now matches `setupRoutes`. Update it
  in the same change whenever you add, remove, or reshape a route.
- **Commits must NOT include `Co-Authored-By: Claude` or any AI-attribution** (user preference).

## Remaining work / known gaps

The BYOK pivot (spec §13) is complete through step 11 and merged to `main`. Open items:

- The post-OAuth `auth_token` cookie is host-only with `SameSite=Lax`, so it only reaches the API on
  cross-origin XHR when the API and frontend share a registrable domain. If the API is deployed somewhere
  like `*.herokuapp.com`, the frontend must use the `Authorization` header.
- `/api/keys` returns `200` with an empty body on upsert/delete; a JSON body would be friendlier.
- **Before public release the owner must (a) rotate the Gemini key leaked on `origin/dev`, and (b) delete
  the stale remote branches `dev`, `critical`.** `main` was reset to a single clean root commit (`08a2b03`)
  to purge a leaked `.env`; old history survives locally on `dev` and the `backup/*-pre-oss-reset` tags.
- **Local LLMs (Ollama / LM Studio) are deferred to v2** — a server cannot reach a user's localhost, so that
  path must be client-side. Don't add server-side local-LLM support without revisiting the spec.
