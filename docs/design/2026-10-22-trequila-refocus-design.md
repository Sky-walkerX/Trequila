# Trequila refocus — "the AI trip planner you can trust", event-driven on GCP

## Context
Trequila (Go backend + `../trequila-frontend` Next.js) does too much at once: itinerary, vibes, photo-to-trip,
replan, budget tiers, two chats, MCP. Visitors can't see the use case. Worse, the frontend sells features
that don't exist: an "ML tourism ranker" and collab voting that calls nonexistent `/api/collab/*` and
`/api/images` routes. The itinerary is also not grounded: planner and places run in parallel, so the LLM
never sees real places.

Research shows the #1 complaint about AI trip planners is **hallucinated or stale facts**. One audit found
90% of ChatGPT itineraries had errors: 52% scheduled places outside opening hours, 24% recommended closed
venues, 25% had illogical routing. Next come **editability** (co-pilot, not autopilot), a map-first view
with realistic travel times, and group planning. Users accept AI that *suggests* (53%); 66% won't let it
book (Expedia AI Trust Gap, Apr 2026).

Goals: (1) a product users get instantly, (2) a portfolio piece showing system design, event-driven
architecture and microservices to SDE intern / new-grad recruiters.

## Decisions (made by the user)
- **Core job:** trustworthy planner for 1–2 travelers. It builds only from real places, respects opening
  hours, uses real travel times, and is editable. **Group planning is phase 2**; the v1 data model must allow it.
- **Keys:** operator free tier on **Gemini 3.8 Flash via Vertex AI** (`gemini-3.8-flash`, ADC, no API key in
  env) with per-user daily limits and a monthly kill-switch. BYOK (Gemini/Anthropic/OpenAI-compatible) stays.
  CLAUDE.md's "no operator keys" rule is rewritten accordingly.
- **ADK (Go)** only for the chat co-pilot. The pipeline uses the existing `ai.LLMProvider` interface.
- **GCP, $20–30/mo:** Cloud Run (one service each) + Cloud SQL Postgres (smallest) + **Pub/Sub push** (replaces
  NATS) + Cloud Scheduler + **Upstash Redis** free. Local dev: docker-compose with the Pub/Sub emulator.
- **Frontend** stays on Vercel. **Alerts** are in-app only (no email yet).
- **v1 extras:** edit-don't-regenerate, pre-trip alerts, .ics calendar + PDF.
- **Cut:** photo→itinerary, budget tiers, LLM "vibes" scores, legacy `/api/trips/chat`, dead code,
  phantom collab UI and ranker claims. **Keep:** MCP (dev-facing).
- **Share:** read-only public links (expiry + revoke); join/co-edit returns in phase 2.
- **Start fresh** on Cloud SQL: no data migration or legacy backfill.
- **Data sources:** upgraded open data (no Google Places). Real travel times via OpenRouteService.
  Better images. Photon destination autocomplete. Map tiles stay on Carto.

## Target architecture — 5 Cloud Run services (one image, separate `cmd/`)
One Cloud SQL DB; **each table has exactly one writer service**. Others read via events or sync APIs,
with documented read-only exceptions (ADR-001).

| Service | Does | Consumes → Produces | Owns |
|---|---|---|---|
| **api** (public) | auth, trips, edit commands/versions/undo, keys, share links, SSE, alerts read, .ics, quota, outbox sweep, DLQ sink, lifecycle status | `route.computed`, `trip.failed`, `dlq`, billing → `trip.requested`, `itinerary.version_created`, `itinerary.regen_requested` | users, provider_keys, trips, trip_members, share_links, itinerary_edits, idempotency_keys, usage_daily, operator_budget, outbox, dead_letters |
| **discovery** (private) | geocode, candidate places + scoring, weather, images, alert checks | `trip.requested`, `alert.check_requested` → `trip.candidates_ready`, `trip.failed`, `alert.raised` | places, trip_candidates, trip_weather, trip_images, alerts |
| **planner** (private) | provider selection, grounded LLM plan, validate/repair, regenerate day, weather replan | `trip.candidates_ready`, `itinerary.regen_requested` → `itinerary.version_created`, `trip.failed` | itinerary_versions, llm_usage, stage_runs |
| **router** (private) | per-day ordering with real travel times + opening hours, conflicts, route geometry | `itinerary.version_created` → `route.computed` | itinerary_routes |
| **agent** (public) | ADK co-pilot `/api/chat` + MCP `/mcp` over one tool registry; every write goes through api | — | ADK session tables |

Layout: `cmd/{api,discovery,planner,router,agent,migrate,devtopology,evalplan}` and
`internal/svc/<service>`, each exposing `Routes(mux)` + `Subscriptions()`. `cmd/monolith` wires all of
them in one process until the GCP cutover.

### Before → after (what each service now does)
- **planner:** wrote the itinerary from nothing → plans **only from candidate place IDs** (enum-constrained structured output), validates, repairs once, then falls back deterministically.
- **places + imager** → merged into **discovery** (scoring, weather, images, alerts).
- **vibes** → deleted. Real Open-Meteo weather moves to discovery.
- **route** (geocoded each activity at ~1/s) → **router**: coordinates come with the candidates; it uses real ORS travel times, schedules around opening hours, flags conflicts, and reruns on every edit version.
- **chatbot + MCP** → **agent**.

## Grounded pipeline
```
POST /api/trips  tx{trip(queued), usage reserve, outbox trip.requested, trip_events} → 202
trip.requested ─► discovery:
   1 geocode (Open-Meteo geocoding w/ timezone; Nominatim fallback, Redis 1 req/s lock)
   2 weather for the trip dates (forecast ≤16 days ahead, else last year's dates labelled "typical")
   3 one combined Overpass query: tourism/historic/leisure/amenity(food) with names; keep opening_hours,
     wikidata, wikipedia, website, fee, indoor, wheelchair → upsert `places`
   4 popularity: Wikidata sitelink count + Wikipedia monthly pageviews (cached 30d)
   5 deterministic score with reason strings: interest match, popularity, distance decay, open on ≥1 trip
     day (closed on all days → excluded), indoor fit for rainy days, dedupe, per-category diversity caps
   6 select ~days×5 sights + days×3 meals → trip_candidates; too few → widen radius once → else trip.failed
   7 images (best-effort, never fails the trip)
   → outbox trip.candidates_ready {compact candidates, tz, weather/day}
trip.candidates_ready ─► planner: stage lease (no double LLM spend) → providerselect → prompt with the
   candidate table + "use ONLY these ids" → validate (internal/grounding) → 1 repair → fallback fill
   → tx{itinerary_versions v1 (stable item_id ULIDs), llm_usage, outbox, trip_events}
itinerary.version_created ─► router: drop stale versions → ORS matrix (walking; driving if >X km)
   → NN + 2-opt (reuse internal/route) unless order_locked → simulate the day from 09:00 → reinsert or flag
   conflicts (closed at arrival / closes before end); meals pinned to windows → ORS directions geometry
   → tx{itinerary_routes, outbox route.computed}
route.computed ─► api: v1 → status 'ready' (completion now gated on the route)
```
**Failures:**
- `bus.Permanent` errors (bad payload, BYOK auth error checked before retryable, too few places) → ack + fail.
- Other errors → nack; Pub/Sub retries with 2–600s backoff; after 5 deliveries → DLQ → api marks the trip failed.
- A minutely reaper fails trips stuck for more than 10 minutes.
- Ack deadlines: discovery 600s, planner 600s, router 120s.

## Data sources (decided)
| Data | Source | Notes |
|---|---|---|
| Places | OSM Overpass, one combined query (~400 candidates) | replaces 3 queries × 5 results; drops hotels |
| Popularity | Wikidata sitelinks + Wikimedia pageviews API | free, explainable "why this place" |
| Opening hours | OSM `opening_hours`, parsed by `internal/hours` (subset grammar) | unparseable → "unknown", never "closed" |
| Closures | OSM `disused:*`/`was:*`/deleted, re-checked in alerts | |
| Travel times | **OpenRouteService** (operator key; 500 matrix/day, 2,000 directions/day) | Redis-cached; falls back to haversine ×1.25 labelled "estimated" when over quota/down |
| Hero image | **Pexels/Unsplash** "{city}" search, filtered by size/aspect; Wikidata P18 fallback | attribution stored + shown |
| Gallery | Wikimedia Commons quality/featured images in the city's category | landscape, ≥1200px, no SVG/maps/logos, license checked |
| Place photos | OSM `wikidata` tag → P18 → Commons (exact match); "nearby" geotagged photo; map crop | moves from the Next.js proxy into discovery |
| Geocode / autocomplete | Open-Meteo geocoding → Nominatim; **Photon** autocomplete via api proxy (cached) | every trip starts from a real place |
| Weather | Open-Meteo forecast + archive | |
| Map tiles | Carto (unchanged) | see risks re: OSM tile-server thumbnails |

## Data model (goose migrations, fresh baseline)
- `cmd/migrate` runs locally and as a Cloud Run Job. Delete `init.sql`, `migrations/` and `bin/migrate`.
- **trips:** status (queued|discovering|planning|routing|ready|failed), current_version, key_source,
  timezone, dest_lat/lon, travelers, interests[], pace. No JSONB result blobs.
- **New tables:**
  - membership and sharing: `trip_members(role)`, `share_links(token, expires_at, revoked_at)`
  - places: `places` (OSM cache), `trip_candidates(score, reasons[], selected)`
  - itinerary: `itinerary_versions(trip_id, version, parent, cause, content jsonb)` with stable day_id/item_id;
    `itinerary_edits` (command log, the phase-2 rebase source); `itinerary_routes(trip_id, version)`
  - enrichment: `trip_weather`, `trip_images`
  - events: `trip_events` + NOTIFY trigger; `stage_runs`; `outbox(headers incl. traceparent)`;
    `processed_events`; `idempotency_keys`; `dead_letters`
  - free tier: `llm_usage`, `usage_daily`, `operator_budget`
  - `alerts(dedupe_key unique, action jsonb)`
- **Why snapshots + op log:** whole-itinerary reads; undo is a pointer move; stable IDs let phase-2
  votes/comments attach to items. Optimistic concurrency: `base_version` must equal current, else 409.

## Messaging, outbox, SSE
- **`internal/bus`:** an `Event` with ID, Type, Subject, TraceParent and Data; `Publisher`; `Handler`;
  `Permanent(err)`. Middleware chain: recover → OTel extract → slog → metrics → idempotent(processed_events) → handler.
  - Adapters: `bus/pubsub` (publisher + push `http.Handler`), `bus/nats` (Phases 1–2 only, replaces the
    6 copy-pasted consumers), `bus/memory` (tests).
  - One topic per event type plus `dlq`. Private services sit behind Cloud Run IAM (OIDC push SA);
    api verifies the OIDC token on `/pubsub/*` and `/internal/*`.
- **Outbox:** written in the same tx; published synchronously after commit (2s budget). A Cloud Scheduler
  minutely sweep (`FOR UPDATE SKIP LOCKED`) also runs the reaper and the processed_events purge.
  Outbox lag is a metric.
- **No ordering keys:** version checks make stale events harmless.
- **SSE via Postgres LISTEN/NOTIFY:**
  - One listener connection per api instance feeding an in-process hub.
  - Subscribe *before* replaying `trip_events` (fixes today's race); `id:` = event id, so Last-Event-ID resumes.
  - Keep the 20s keepalive. Cloud Run api runs with timeout 3600, concurrency 250, max 2 instances;
    the server caps a stream at 10 min, then the client reconnects.
- **Upstash** is used only for the cache (with an in-memory LRU in front), the Nominatim lock and the Overpass semaphore.

## Free tier
- **`providerselect.Resolve`:** the user's BYOK default → operator Vertex if enabled and under quota →
  `ErrQuotaExceeded` (429 + BYOK CTA) or `ErrFreeTierPaused` (503).
- **Gemini adapter:** rewritten on `google.golang.org/genai` (Gemini API or Vertex backend), replacing the
  deprecated `generative-ai-go`. Usage is recorded via `ai.WithUsageRecorder(ctx)` → `llm_usage`.
- **Quota:** an atomic conditional `UPDATE usage_daily` in the same tx as the trip/edit/chat. Planner
  re-checks `operator_budget.disabled` before calling Vertex.
- **Kill-switch:** app-level monthly ($10) + daily caps, with a GCP Billing budget → Pub/Sub → api as backstop.
- **Limits:** 3 trips, 10 day-regens and 30 co-pilot messages per user per day (~$0.016 per trip).

## Edit, don't regenerate
- **Endpoints:**
  - `GET /api/trips/{id}`, plus `/versions`, `/versions/{v}` and `POST /versions/{v}/restore`
  - `GET /items/{item_id}/alternatives` (deterministic: unused nearby candidates open at that slot)
  - `POST /api/trips/{id}/edits {base_version, op, args}` with an Idempotency-Key header
- **Ops:**
  - sync (pure `internal/itinerary.Apply`, property-tested): move/swap/add/remove/pin item, set day window
  - async via planner: `regenerate_day`, and `weather_replan` (regenerate with indoor preference;
    replaces `/api/trips/replan`)
  - every new version → router → SSE `version` then `route`. `/api/trips/optimize` is deleted.
- **Tool registry** (`internal/tools`, generic `Register[A,R]`, schema via google/jsonschema-go): one
  registry serves MCP (`mark3labs`) and ADK (`functiontool`, RequireConfirmation for destructive tools).
  Handlers call api through `internal/apiclient` with the user's JWT. `delete_trip`/`set_api_key`/
  `remove_api_key` stay MCP-only.
- **ADK models:** `llm.NewADKModel`: Gemini/Vertex → ADK gemini; OpenAI-compatible → ADK openaimodel;
  Anthropic → `adkbridge` over `ChatWithTools`.

## Pre-trip alerts (in-app)
- **Trigger:** Cloud Scheduler every 6h → discovery `/jobs/alerts/sweep` → one `alert.check_requested`
  for each ready trip starting within 16 days.
- **Weather checks:** rain ≥60% on a day with ≥2 outdoor items → action `weather_replan(day)`;
  storm or heat alerts.
- **Closure checks:** an Overpass by-ID re-check flags a deleted element, `disused` tags, changed hours,
  or a place now closed on its scheduled weekday → action `swap_item` with prefetched alternatives.
- **Dedupe + resolve:** `dedupe_key` dedupes; alerts whose condition clears auto-resolve.
- **Surfacing:** trip_events → SSE. API: `GET /api/alerts`, `/api/trips/{id}/alerts`, dismiss/seen.

## Frontend (`../trequila-frontend`)
- **Remove:**
  - phantom features: `lib/collab.ts`, `CollabSession`, `JoinTrip`, `RankerReadout`, the hero
    ranker/collab claims, ranker fields in `ItineraryViewer`
  - cut features: `PhotoUpload`, `TripChat`, the vibe UI in `PlacesShowcase`, the `/api/images` call
  - landing leftovers: static London `lib/trip-data.ts` (keep types → `lib/types.ts`), the fake landing
    form, the console contact form, dead components, axios
- **Routes:**
  - `/trips`, `/trips/new` (Photon autocomplete; draft saved and handed off through `/auth?next=`)
  - `/trips/[id]`: progress, itinerary, map, weather and alerts; replaces the Dashboard view state machine
  - `/s/[token]` (SSR read-only + OG image), `/examples/[slug]` (real generated trips)
  - `/settings/keys` (keys + usage meter)
- **Viewer:**
  - item_id keys, a verified-place badge + OSM link, "why this place" built from `reasons[]`
  - hours line and route conflict warnings; real travel time per leg (labelled "estimated" on fallback)
  - per-day weather and image attributions
- **Edit UI:** item menu (swap via alternatives drawer, remove, pin, move); drag-reorder with `motion`
  Reorder (already a dependency); optimistic update with 409 rollback; regenerate-day sheet; version
  history/undo; docked co-pilot with confirm/undo.
- **Also:** alerts bell + banners; usage meter + 429 BYOK CTA; `.ics` button; PDF updated to the new model.
- **Honest landing:** "Itineraries built only from verified OpenStreetMap places, checked against opening
  hours and real travel times", a working CTA, a how-it-works pipeline diagram, real example trips.

## Roadmap — each phase ships on its own (S≈2–3d, M≈1wk, L≈2wk)
0. **Prep + cut (M, still on Heroku). DONE — this repository starts from the post-cut baseline.**
   - Delete photo/budget-tiers/trip-chat/join handlers, `internal/vibes` + its worker/consumer,
     `AnalyzeImage`/`ErrVisionUnsupported`, `ai.Config`/`gemini.New`, `bin/migrate`.
   - SSE completion = itinerary + 30s places grace. Add the missing `defer Close()` in planner/chatbot.
   - Frontend removals. Update `API_DOCUMENTATION.md` + CLAUDE.md.
1. **Foundations, still one binary (M–L).**
   - goose baseline, slog, `internal/bus` (NATS + memory adapters) collapsing the 6 consumers.
   - Split `main.go` into `internal/svc/*`.
   - outbox + relay, processed_events, idempotency_keys, trip_events + LISTEN SSE hub, trip_members.
   - Full docker-compose. GitHub Actions (fmt, vet, golangci-lint, `test -race` against a Postgres
     container, migrations up/down, frontend build). ADRs 001–004.
2. **Grounded pipeline (L) — the core value.**
   - Discovery: combined Overpass, Wikidata popularity, scoring, `internal/hours`, image upgrades.
   - Planner: candidate-ID prompt + validator/repair. Router: ORS + hours-aware scheduling.
   - New viewer model + `/trips/[id]`, Photon autocomplete.
   - `cmd/evalplan` report (invalid-ID rate before/after repair, hours violations, km/day,
     time-to-ready) → `docs/eval/`. ADRs 005–006.
3. **GCP cutover (M–L).**
   - `bus/pubsub` + OIDC, 4 `cmd/` services, one Dockerfile (`ARG SERVICE`, distroless + tzdata).
   - Terraform: Cloud SQL, topics/subs/DLQ/IAM, Cloud Run with per-service SAs, Scheduler, Secret
     Manager, Artifact Registry cleanup, Billing budget.
   - OTel (otelhttp, otelpgx, traceparent through outbox/Pub/Sub → Cloud Trace; Jaeger locally).
   - Compose with the Pub/Sub emulator + `devtopology`. Domain `api.trequila.namankhandelwal.dev`.
   - Delete `internal/nats`, Procfile, app.json, keep-alive. CI deploys via Workload Identity
     Federation + the migrate job. ADRs 007–008.
4. **Free tier + share links + honest landing (M):** genai/Vertex provider, usage recorder, quota,
   kill-switch, `share_links` + `/s/[token]`, `/api/examples`, landing rewrite. ADR-009.
5. **Edit, don't regenerate (L):** `internal/itinerary` ops, edits/alternatives/versions endpoints,
   regenerate_day + weather_replan, edit UI.
6. **ADK co-pilot + single registry (M–L):** `internal/tools`, `cmd/agent` (moves `/mcp`, `/api/chat`),
   ADK models/bridge, confirmations, DB sessions, chat quota. Delete `internal/chatbot` and `conversations`. ADR-010.
7. **Alerts + calendar (M):** alerts tables/jobs/endpoints/UI, `internal/ics` (+ webcal on share links), PDF refresh.
8. **Portfolio hardening (M):**
   - k6 on staging with a fake LLM + `cmd/fakeosm`: read p95, create-burst time-to-ready / outbox lag /
     DLQ = 0, SSE connections per instance.
   - C4 + sequence diagrams, per-service Postgres GRANTs, runbook, Cloud Monitoring dashboard, README rewrite.

**Stopping points:** after Phase 2 there's a trustworthy planner; after Phase 3 it's event-driven on GCP;
after Phase 4 it's usable without a key.

Each phase gets its own implementation plan under `docs/plans/` and is built one at a time.

## Risks / guardrails
- **Cloud SQL smallest tier ≈25 connections:** per-service pool budget (api 2×(4+1), discovery 3,
  planner 4, router 2, agent 4 = 23). Never hold a connection during an LLM call or SSE. The next tier
  (~$25) breaks the budget.
- **Cold starts across a 4-hop chain (~1–2s):** cpu-boost, distroless; api min-instances=1 only for demos.
- **Delivery limits:** Pub/Sub is at-least-once → processed_events + stage leases everywhere. The push
  ack deadline caps at 600s.
- **Overpass/Nominatim on shared Cloud Run egress IPs:** Redis semaphore, 30-day place cache, mirror
  failover, prewarmed example cities, Open-Meteo geocoding first.
- **ORS free limits (500 matrix/day):** cache matrices; fall back to an honest "estimated" haversine.
- **OSM opening_hours coverage is patchy** → show "unknown" rather than guess.
- **Map thumbnails use the public OSM tile server** (`app/api/map-thumb`), which its usage policy forbids
  at app scale. Point `MAP_TILE_URL` at a permitted source or replace with a lazy mini-map before launch.
- **Cost:** ~$17–28/mo (Cloud SQL ~$12, IPv4 ~$3.6, LLM capped $10, rest ~$0). Budget alerts at $25/$30;
  max-instances on every service.
- **Pin versions:** `gemini-3.8-flash` (config value) and the ADK Go version.
- **Commits:** follow CLAUDE.md — **no AI attribution** (overrides the default trailer).

## Verification (per phase)
- Always: `go build ./... && go vet ./... && go test ./...`, `gofmt -l .`; frontend `next build` + eslint.
- Phase 0: grep for collab/ranker/vibes/budget-tiers/photo returns nothing; one manual trip end-to-end.
- Phase 1:
  - integration tests on real Postgres: a duplicate delivery writes once; a crash between commit and
    publish is recovered by the sweep
  - compose e2e: create a trip → stream it to ready
- Phase 2:
  - golden tests for scoring/hours/validator/router; a fake LLM injecting bad IDs gets repaired
  - httptest fixtures for Overpass/Wikidata/ORS/Pexels
  - eval report on ~10 cities
- Phase 3: compose e2e on the emulator; staging smoke; forced planner failure → retries → DLQ → failed
  trip with a reason; one trace across 4 services.
- Phase 4: 10 parallel creates with limit 3 → exactly 3; forced budget cap → 503 banner; real Vertex smoke.
- Phases 5–7:
  - property tests on `Apply`; 409/idempotency API tests; reorder → new version → route in <2s
  - fake ADK model scripts tool calls; prompt-injection test
  - `.ics` validates and imports into Google Calendar
- Phase 8: published k6 numbers.

## Sources
- Expedia AI Trust Gap: https://ir.expediagroup.com/news-and-events/news/news-details/2026/Expedia-Group-Reveals-The-AI-Trust-Gap-Travelers-Embrace-AI-for-Planning-but-Rely-on-Trusted-Brands-to-Book/default.aspx
- 90% inaccurate itineraries: https://travolution.com/news/marketing/survey-finds-chatgpt-itineraries-to-be-riddled-with-inaccuracies
- Failure modes: https://tripprof.com/uk/blog/why-ai-cant-plan-your-trip/
- 20-app comparison: https://tripprof.com/en/blog/best-trip-planning-apps-2026/
- AI planner comparison: https://voyaige.to/blog/best-ai-travel-planner-2026
- Google Places SKUs: https://developers.google.com/maps/documentation/places/web-service/place-details ; pricing https://developers.google.com/maps/billing-and-pricing/pricing ; terms https://cloud.google.com/maps-platform/terms/maps-service-terms
- OpenRouteService plans: https://openrouteservice.org/plans/
- Gemini 3.8 Flash on Vertex: https://docs.cloud.google.com/vertex-ai/generative-ai/docs/models/gemini/3-8-flash
- Overture places: https://docs.overturemaps.org/guides/places
