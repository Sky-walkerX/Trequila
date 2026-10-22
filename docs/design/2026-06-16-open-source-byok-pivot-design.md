# Trequila — Open-Source BYOK Pivot (v1)

**Status:** Approved design · **Date:** 2026-06-16 · **License target:** Apache-2.0

## 1. Summary

Trequila pivots from a closed business product (which used the operator's own Gemini key)
into an **open-source, bring-your-own-key (BYOK) trip-planning visualizer**. Users supply
their own LLM API key; the app generates and stores their trips. The existing event-driven
NATS architecture is preserved.

**v1 scope (this spec):** cloud LLM providers only, with user keys stored server-side
(encrypted at rest), and **fully keyless enrichment** (images + places) so the hosted
instance has no rate limits and no operator API keys at all.

**Explicitly out of scope for v1 (deferred to v2):**

- **Local LLMs (Ollama / LM Studio).** A server-side worker cannot reach a user's
  `localhost`, so local models require a *client-side* generation path. The provider
  abstraction is designed so this drops in later (the OpenAI-compatible adapter already
  matches Ollama's API shape; only the call site moves to the browser).
- The frontend (separate concern/repo), billing, team/org accounts, per-user key
  derivation / envelope encryption with a KMS.

## 2. Goals & non-goals

**Goals**

1. Remove all operator-owned third-party credentials from the product and repo.
2. Let a user paste **one LLM key** (or configure an OpenAI-compatible endpoint) and get a
   full trip — itinerary, images, places, vibes.
3. Support **Google Gemini (native), Anthropic Claude (native), and any OpenAI-compatible
   endpoint** behind a single provider abstraction.
4. Make enrichment (images/places) **free, keyless, and unthrottled** via open data sources.
5. Surface provider failures (bad/expired key, no credits, quota) clearly instead of
   silently retrying.
6. Ship as a clean, self-hostable Apache-2.0 project.

**Non-goals**

- Changing the NATS/JetStream event model (it stays).
- Guaranteeing identical image/POI quality to the previous Pexels/Geoapify data (we accept
  a modest quality trade-off for keyless/free).

## 3. Architecture

The pipeline is unchanged in shape. The only structural change: workers that call an LLM
(**Planner**, **Vibes**) stop reading a global env key and instead resolve the **requesting
user's** encrypted key from the database. Workers that only need a destination string
(**Imager**, **Places**) no longer use any key — they call keyless open APIs.

```
POST /api/trips  (JWT → user_id)
  1. resolve user's default (or request-specified) provider + model
  2. if no usable key for that provider → 400 "add an API key in Settings" (before queuing)
  3. snapshot {provider, model} onto the trips row; insert status='pending'
  4. publish trips.requested  (envelope now carries user_id, provider, model)

  trips.requested fans out to:
    Planner → resolve user key (user_id, provider) → user's LLM → trips.itinerary
    Places  → OSM Nominatim geocode + Overpass POIs (keyless) → trips.places
    Imager  → Wikipedia REST + Openverse (keyless)            → trips.images
  Vibes (triggered by trips.places) → resolve user key → user's LLM → trips.vibes

  consumers persist JSONB columns; SSE /api/trips/stream/{id} streams partials (unchanged)
  any worker hitting an auth/credit/quota error → trips.failed
       → status='failed' + error_reason; SSE emits a `failed` event
```

**Event context propagation (correctness fix).** Today `trips.places` does not carry the
user. Since Vibes needs the user's key, every event in the chain MUST carry a minimal trip
context `{request_id, user_id, provider, model}`. `trips.requested` already has `user_id`
(via `TripRequest`); the Places worker copies this context into `trips.places` so the Vibes
worker is self-contained and never has to guess the user. No worker reads another worker's
DB rows to find the user.

## 4. LLM provider abstraction

Generalize the existing `ai.ItineraryGenerator` into a capability-based `LLMProvider`
interface in `internal/ai`, with one adapter per provider and a factory that builds the
right adapter from a resolved provider config.

```go
// internal/ai/provider.go (design-level signature)
type ProviderKind string // "gemini" | "anthropic" | "openai_compatible"

type ProviderConfig struct {
    Kind    ProviderKind
    APIKey  string // decrypted, in-memory only
    BaseURL string // required for openai_compatible; ignored for native
    Model   string
}

type LLMProvider interface {
    // Structured itinerary generation (Planner).
    GenerateItinerary(ctx context.Context, trip types.TripRequest) ([]types.DayPlan, error)
    // Generic JSON-mode generation (replan, budget tiers, vibes, photo analysis).
    GenerateJSON(ctx context.Context, prompt string) (string, error)
    // Multi-turn chat (trip chat).
    Chat(ctx context.Context, system string, history []ChatTurn, msg string) (string, error)
    // Vision (photo → trip). Capability-gated: returns ErrVisionUnsupported if the
    // configured model can't accept images.
    AnalyzeImage(ctx context.Context, img []byte, mime, prompt string) (string, error)
    Close() error
}

func NewProvider(ctx context.Context, cfg ProviderConfig) (LLMProvider, error)
```

**Adapters (`internal/ai/{gemini,anthropic,openai}`):**

- `gemini` — refactor the current `genai` implementation behind the new interface.
- `anthropic` — native Messages API adapter.
- `openai` — OpenAI-compatible (`POST {baseURL}/chat/completions`); serves OpenAI,
  OpenRouter, Groq, Together, Azure OpenAI, and (v2) local Ollama/LM Studio.

**Shared helpers (kills current duplication):** one `cleanJSON()` (replaces the ~4 copies
of "slice from first `{`/`[` to last `}`/`]`") and one retry/backoff helper (currently
duplicated in `gemini.go` and `vibes/ai.go`). Both live in `internal/ai`.

**Capability notes:** vision is required only by photo→trip; if a user's model lacks it,
that endpoint returns a clear 422. JSON-mode: native providers use response-format flags;
the OpenAI adapter uses `response_format: json_object` when supported and falls back to
prompt-enforced JSON + `cleanJSON()`.

## 5. Key management

**Storage:** new table, one row per (user, provider):

```sql
CREATE TABLE provider_keys (
    user_id        TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider       TEXT NOT NULL,        -- 'gemini' | 'anthropic' | 'openai_compatible'
    encrypted_key  TEXT NOT NULL,        -- base64(nonce || AES-256-GCM ciphertext)
    base_url       TEXT,                 -- for openai_compatible
    default_model  TEXT NOT NULL,
    is_default     BOOLEAN NOT NULL DEFAULT FALSE,
    created_at     TIMESTAMPTZ DEFAULT NOW(),
    updated_at     TIMESTAMPTZ DEFAULT NOW(),
    PRIMARY KEY (user_id, provider)
);
-- exactly one default per user enforced in app logic (set others false on write)
```

**Encryption:** AES-256-GCM. Master key from a new `ENCRYPTION_KEY` env var (32 bytes,
base64-encoded). Fresh random 12-byte nonce per write, stored as `nonce||ciphertext`
(base64). `internal/crypto` exposes `Encrypt(plaintext) (string, error)` /
`Decrypt(stored) (string, error)`. Server **fails to start** if `ENCRYPTION_KEY` is missing
or wrong length (fail closed). Documented for self-hosters: lose/rotate this key →
stored provider keys become unreadable and must be re-entered.

**API (all under JWT auth):**

| Method | Path | Behavior |
|---|---|---|
| `GET` | `/api/keys` | List configured providers: `{provider, masked: "sk-…last4", base_url, default_model, is_default}`. **Never** returns plaintext. |
| `POST` | `/api/keys` | Upsert a provider key `{provider, api_key, base_url?, default_model, make_default?}`. Encrypts and stores. |
| `POST` | `/api/keys/test` | Cheap validation call to the provider with the supplied key; returns ok/why. Used before save. |
| `DELETE` | `/api/keys/{provider}` | Remove a stored key. |

**Resolution service (`internal/providerkeys`):**
`Resolve(ctx, userID, overrideProvider string) (ai.ProviderConfig, error)` → loads the row
(default or override), decrypts the key in memory, returns config. Returns
`ErrNoKey` when the user has none. The decrypted key is never logged and never persisted
beyond the call.

**Request-time vs worker-time:** at trip creation we validate the user has a usable key and
**snapshot `{provider, model}`** onto the trip + event. Workers re-resolve the **key** fresh
at processing time by `(user_id, provider)`. If the key was deleted between request and
processing → `trips.failed` with a clear reason.

## 6. Keyless enrichment

Replace Geoapify and Pexels with open, keyless sources. Keep the existing Redis caching
(7-day TTL) and the existing worker/event structure and JSONB columns — only the HTTP
clients inside the services change.

**Places (`internal/geo`):**

- Geocode: **OSM Nominatim** (`/search`). Requires a descriptive `User-Agent`
  (`Trequila/1.0 (+repo-url)`); respect ~1 req/s — cache absorbs the rest.
- POIs: **OSM Overpass** (`node/way[...](around:radius,lat,lon)`), mapped to the existing
  hotel/restaurant/attraction categories.
- Keep the negative-cache behavior but **drop the (0,0) sentinel** (it falsely rejects
  equatorial coords); cache a typed `{found:false}` marker instead.
- Self-host note: operators expecting scale can point `OSM_NOMINATIM_URL` /
  `OSM_OVERPASS_URL` at their own instance (env-overridable base URLs); defaults are the
  public endpoints.

**Images (`internal/images`):**

- Hero image: **Wikipedia REST** `/page/summary/{destination}` → `thumbnail`/`originalimage`.
- Gallery: **Openverse** image search (`/v1/images?q=`) for CC-licensed variety; optional
  **Wikimedia Commons** geosearch around the destination coords for authentic local photos.
- Same `Image{url, photographer/attribution, alt}` output shape; include CC attribution.
- All keyless; only a descriptive `User-Agent` is needed.

**Result:** zero operator keys for enrichment, no hard quotas. The image-source code is
structured so a future Pexels adapter (v-next) can be enabled via env without touching
callers.

## 7. Failure & status model

Add to `trips`: `provider TEXT`, `model TEXT`, `error_reason TEXT`, and allow
`status='failed'`.

Workers classify errors:

- **Permanent (fail fast, no retry):** 401/403 (bad/expired key), 402/quota/insufficient
  credits, model-not-found, vision-unsupported. → publish `trips.failed`
  `{request_id, stage, reason}`; a consumer sets `status='failed'`, `error_reason=reason`.
- **Transient (retry as today):** network, 429 rate-limit on the provider, 5xx, NATS issues.
  Keep the existing backoff/Nak path.

SSE (`/api/trips/stream/{id}`) gains a `failed` event so the UI stops waiting and shows the
reason. The 2-minute stream timeout and vibes grace window are unchanged for the success
path.

## 8. Configuration changes

**Removed env vars:** `GEMINI_API_KEY`, `GEMINI_VIBES_KEY`, `PEXELS_API_KEY`,
`GEOAPIFY_API_KEY` (all now BYOK or keyless).

**Added:** `ENCRYPTION_KEY` (required, 32-byte base64).

**Kept:** `JWT_SECRET` (now **required** — fail closed if unset, removing the weak
hardcoded default in `auth.go`), `DATABASE_URL`, `REDIS_URL`, `NATS_URL`/creds, optional
Google OAuth vars.

**Added (optional, with public defaults):** `OSM_NOMINATIM_URL`, `OSM_OVERPASS_URL`,
`WIKIPEDIA_API_URL`, `OPENVERSE_API_URL`, `HTTP_USER_AGENT`.

`docker-compose.yml`, `app.json`, and `.env.example` updated accordingly.

## 9. Database migration

Schema lives in `init.sql` (canonical) plus an additive migration for existing
deployments:

```sql
ALTER TABLE trips ADD COLUMN IF NOT EXISTS provider TEXT;
ALTER TABLE trips ADD COLUMN IF NOT EXISTS model TEXT;
ALTER TABLE trips ADD COLUMN IF NOT EXISTS error_reason TEXT;
CREATE TABLE IF NOT EXISTS provider_keys ( ... as §5 ... );
```

Existing trips (no provider/model) remain valid as historical records.

## 10. Open-source hygiene & scaffolding

- Git history already reset to a clean root commit on `main` (leaked `.env` removed from the
  new lineage; `dev`/backups retained locally and to be deleted by the owner before public
  release). **All credentials that ever appeared in the old `.env` must be rotated** at the
  provider regardless, since they remain in pushed branches until those are deleted.
- Add `LICENSE` (Apache-2.0) + `NOTICE`.
- Rewrite `README.md`: what it is, BYOK quickstart ("paste one LLM key"), self-host guide
  (env vars, `ENCRYPTION_KEY` generation, NATS/Postgres/Redis), architecture overview.
- Add `CONTRIBUTING.md` and `SECURITY.md` (how to report vulns; the key-custody model).
- Rewrite `.env.example` for the new variable set.
- Delete the empty `Caddyfile`; remove the stale local binaries from the working tree
  (`monolith`, `bin/migrate` — already gitignored).

## 11. Security hardening (required before public launch)

Going from a single-operator product to a public, open-source, multi-tenant instance turns
several latent issues into real risks. The following were confirmed by automated security
review of the baseline commit and are in scope for v1:

- **[CRITICAL] Remove/close the `/auth/token` dev endpoint.** It currently mints a valid JWT
  for an arbitrary `user_id` with no authentication → account takeover on a public instance.
  Default-closed behind `ENABLE_DEV_TOKENS=true` (off in prod), and ensure the
  `ON CONFLICT DO UPDATE` cannot overwrite another user's record.
- **[CRITICAL] No hardcoded JWT secret.** `JWT_SECRET` required at startup, reject if empty
  or `< 32` bytes (also see §8).
- **[HIGH] OAuth CSRF / state verification.** Store the OAuth `state` in a short-lived
  HttpOnly+Secure+SameSite=Lax cookie on `/auth/google/login`; verify it on callback and
  reject mismatches.
- **[HIGH] Redis TLS verification.** Replace blanket `InsecureSkipVerify: true` (geo +
  images) with proper verification — set `ServerName` from the parsed `REDIS_URL` host and/or
  attach the correct intermediate CA. (These files are rewritten in §6 anyway.)
- **[MEDIUM] Token delivery off the URL.** Deliver the post-OAuth JWT via HttpOnly cookie or
  URL fragment instead of a `?token=` query param (avoids Referer/proxy-log leakage).
- **[MEDIUM] Trusted-proxy rate limiting.** Only honor `X-Forwarded-For` from known proxy
  CIDRs (or take the right-most entry on Heroku); otherwise key on `RemoteAddr`.
- **[MEDIUM] Stronger share codes.** Replace `uuid[:8]` with 16+ bytes of `crypto/rand`
  (base32); consider expiry.

## 12. Targeted code-quality cleanups

Only code we are already rewriting:

- Collapse duplicated Gemini JSON-cleanup + retry into `internal/ai` shared helpers (§4).
- Remove dead, never-incremented metrics (`TripsCompletedTotal`, `SSEEventsTotal`,
  `NatsMessagesPublishedTotal`, `NatsMessagesConsumedTotal`) or wire them up; wire
  `TripsCompletedTotal` on itinerary save and add a `TripsFailedTotal`.
- Remove dead `auth.OptionalAuthMiddleware`, `auth.GetClaimsFromContext`, unused
  `types.Itinerary` if still unused after the refactor.
- Use a private context-key type in `auth` (replaces string keys at `auth.go:162-163`).
- Replace `middleware.joinStrings`/`intToString` with `strings.Join`/`strconv.Itoa`.
- Standardize `internal/images` logging on `log` (drop `fmt.Println`).

Out of scope: extracting all handlers/workers out of the 2,462-line `main.go`. We will move
the **new** code (key endpoints, provider factory, worker key-resolution) into packages and
only minimally touch existing handlers where the global-key removal forces it.

## 13. Suggested implementation order

1. `internal/crypto` (AES-GCM) + `ENCRYPTION_KEY` startup validation.
2. `provider_keys` schema + migration.
3. `LLMProvider` interface + factory + the three adapters + shared `cleanJSON`/retry;
   refactor existing `gemini` behind it.
4. `internal/providerkeys` resolution service + `/api/keys` endpoints + validation.
5. Wire Planner + the 5 sync features to resolve per-user keys; remove global LLM key.
6. Event-context propagation (`user_id, provider, model`) through to Vibes; wire Vibes.
7. Failure/status model + SSE `failed` event + metrics.
8. Keyless enrichment rewrite (geo → OSM, images → Wikipedia/Openverse).
9. Config/compose/.env.example updates; remove old vars.
10. Security hardening pass (§11): close `/auth/token`, OAuth state, Redis TLS, trusted-proxy
    rate limiting, stronger share codes.
11. Open-source scaffolding (LICENSE, README, CONTRIBUTING, SECURITY) + stale-file cleanup.

## 14. Risks & mitigations

- **Public OSM/Wikipedia fair-use limits.** Mitigated by existing 7-day cache + descriptive
  User-Agent + env-overridable self-hosted endpoints.
- **Key custody liability** (operator stores users' billing credentials). Mitigated by
  AES-256-GCM at rest, master key outside the DB, never logging/returning plaintext, delete
  support, and clear `SECURITY.md` disclosure. Self-hosters own their own risk.
- **Provider API drift** (Anthropic/OpenAI schema changes). Isolated to single adapters.
- **Vision capability varies by model.** Capability-gated with a clear 422.
- **Old leaked key remains in pushed branches** until `dev`/`critical` are deleted on
  GitHub. Mitigation: rotate the key (mandatory) and delete those branches before launch.
