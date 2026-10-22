# Trequila API Documentation

An open-source, bring-your-own-key (BYOK) trip planning API with real-time updates via Server-Sent Events (SSE).

Every AI call uses **the caller's own LLM API key**, stored encrypted server-side. Add a key via
[`/api/keys`](#provider-keys-byok) before creating a trip, or the request is rejected with
`add an API key in Settings`.

## Base URL

- **Development:** `http://localhost:8080`
- **Production:** your own deployment (the reference frontend is `https://trequila.namankhandelwal.dev`)

---

## Authentication

All `/api/*` endpoints require JWT authentication.

### Header Format
```
Authorization: Bearer <jwt_token>
```

### Getting a Token

#### Option 1: Register with Email/Password
```http
POST /auth/register
Content-Type: application/json

{
  "email": "user@example.com",
  "password": "securepassword123",
  "name": "John Doe"
}
```

**Response (201 Created):**
```json
{
  "token": "eyJhbGciOiJIUzI1NiIs...",
  "user": {
    "id": "550e8400-e29b-41d4-a716-446655440000",
    "email": "user@example.com",
    "name": "John Doe"
  }
}
```

**Errors:**
- `400` - Invalid email or password too short (min 8 chars)
- `409` - User already exists

---

#### Option 2: Login with Email/Password
```http
POST /auth/login
Content-Type: application/json

{
  "email": "user@example.com",
  "password": "securepassword123"
}
```

**Response:**
```json
{
  "token": "eyJhbGciOiJIUzI1NiIs...",
  "user": {
    "id": "550e8400-e29b-41d4-a716-446655440000",
    "email": "user@example.com",
    "name": "John Doe"
  }
}
```

**Errors:**
- `401` - User not found or invalid password

---

#### Option 3: Google OAuth
```
GET /auth/google/login
```
Redirects to Google. `/auth/google/login` sets a short-lived `oauth_state` cookie which
`/auth/google/callback` verifies (CSRF protection); a mismatch returns `400 invalid oauth state`.

On success the callback sets the JWT as an **HttpOnly, Secure, SameSite=Lax `auth_token` cookie** — the
token is never placed in the URL — and redirects the browser to `FRONTEND_URL` (`/auth?error=auth_failed`
on failure). Requests may authenticate with either the `Authorization` header or that cookie.

---

> **Note:** The old unauthenticated `POST /auth/token` dev endpoint has been removed. Register or log in
> to obtain a token. `JWT_SECRET` must be set to at least 32 bytes or the server refuses to start.

---

### Get Current User

```http
GET /api/me
Authorization: Bearer <token>
```

**Response:**
```json
{
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "email": "user@example.com",
  "name": "John Doe",
  "provider": "local",
  "avatar_url": "",
  "created_at": "2024-12-11T10:00:00Z"
}
```

---

## API Endpoints

### Health Check

```http
GET /health
```

**Response:**
```json
{
  "status": "healthy",
  "timestamp": "2024-12-11T10:00:00Z",
  "services": {
    "database": "healthy",
    "nats": "healthy"
  }
}
```

### Metrics

```http
GET /metrics
Authorization: Bearer <METRICS_TOKEN>
```

Prometheus metrics. The endpoint is **not served** (404) unless the operator sets `METRICS_TOKEN`, and then it
requires that token as a bearer token.

---

### Provider Keys (BYOK)

Trequila never ships an operator LLM key. Each user stores their own, encrypted at rest with AES-256-GCM.
Plaintext keys are **never** returned by the API.

Supported `provider` values: `gemini`, `anthropic`, `openai_compatible`.

#### List Keys

```http
GET /api/keys
Authorization: Bearer <token>
```

**Response:** one entry per configured provider, with the key masked.
```json
[
  {
    "provider": "gemini",
    "masked_key": "...7Xc2",
    "default_model": "gemini-2.5-flash",
    "is_default": true
  }
]
```

#### Add / Update a Key

```http
POST /api/keys
Authorization: Bearer <token>
Content-Type: application/json

{
  "provider": "anthropic",
  "api_key": "sk-ant-...",
  "base_url": "",
  "default_model": "claude-sonnet-4-6",
  "make_default": true
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `provider` | string | ✅ | `gemini`, `anthropic`, or `openai_compatible` |
| `api_key` | string | ✅ | Required except for `openai_compatible` (local/keyless endpoints) |
| `base_url` | string | ❌ | Required for `openai_compatible`. Must be `https` and must not point at a private or internal address (loopback, RFC 1918, link-local, cloud metadata); otherwise `400`. Operators who run a model on their own network set `ALLOW_PRIVATE_PROVIDER_URLS=true`. |
| `default_model` | string | ✅ | Model used when a request does not override it |
| `make_default` | bool | ❌ | Make this the user's default provider (at most one) |

**Response:** `200 OK` (empty body). Upsert is keyed on `(user, provider)`.

#### Test a Key

```http
POST /api/keys/test
Authorization: Bearer <token>
Content-Type: application/json

{
  "provider": "anthropic",
  "api_key": "sk-ant-...",
  "model": "claude-sonnet-4-6"
}
```

Performs one cheap live call against the provider. Omit `api_key` to re-test the key already stored for
that provider.

**Response:**
```json
{ "ok": true }
```
| Status | Meaning |
|--------|---------|
| 200 | Key works |
| 401 | Provider rejected the credentials (invalid key / no credit) |
| 400 | Bad request, unknown provider, or no key configured |
| 502 | Provider unreachable or errored |

#### Delete a Key

```http
DELETE /api/keys/{provider}
Authorization: Bearer <token>
```

**Response:** `200 OK`

---

### Trips

#### List User's Trips

```http
GET /api/trips?page=1&limit=10
Authorization: Bearer <token>
```

**Query Parameters:**
| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `page` | int | 1 | Page number |
| `limit` | int | 10 | Items per page (max 50) |

**Response:**
```json
{
  "trips": [
    {
      "request_id": "550e8400-e29b-41d4-a716-446655440000",
      "source": "New York",
      "destination": "Paris",
      "start_date": "2024-06-01",
      "end_date": "2024-06-07",
      "budget": 3000,
      "status": "completed",
      "created_at": "2024-05-15T10:30:00Z"
    }
  ],
  "pagination": {
    "page": 1,
    "limit": 10,
    "total": 25,
    "total_pages": 3
  }
}
```

---

#### Create Trip

```http
POST /api/trips
Authorization: Bearer <token>
Content-Type: application/json

{
  "source": "New York",
  "destination": "Paris",
  "start_date": "2024-06-01",
  "end_date": "2024-06-07",
  "budget": 3000,
  "interests": ["museums", "food", "architecture"],
  "provider": "anthropic",
  "model": "claude-sonnet-4-6"
}
```

**Request Body:**
| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `source` | string | ✅ | Starting location |
| `destination` | string | ✅ | Destination city |
| `start_date` | string | ✅ | Format: `YYYY-MM-DD` |
| `end_date` | string | ✅ | Format: `YYYY-MM-DD` |
| `days` | int | ❌ | Defaults to the inclusive span between the dates |
| `budget` | int | ❌ | Trip budget in USD |
| `interests` | string[] | ❌ | User interests for personalization |
| `provider` | string | ❌ | Override the user's default provider for this trip |
| `model` | string | ❌ | Override the provider's default model for this trip |

**Idempotency.** Send an `Idempotency-Key` header (any string up to 255 characters, unique per logical request)
to make retries safe. Repeating the request with the same key and body returns the original `request_id` with
the header `Idempotent-Replayed: true` and creates nothing. Reusing a key with a *different* body returns
`422`. Keys are scoped to the user.

The resolved `{provider, model}` is snapshotted onto the trip; workers re-resolve the key at processing
time. If the user has no key for the requested provider the request fails with
`400 {"error":"add an API key in Settings"}`.

**Response (202 Accepted):**
```json
{
  "request_id": "550e8400-e29b-41d4-a716-446655440000",
  "status": "pending",
  "message": "Trip request accepted"
}
```

> ⚡ **Important:** After creating a trip, connect to the SSE stream to receive real-time updates as the itinerary is generated.

---

#### Get Trip Details

```http
GET /api/trips/{request_id}
Authorization: Bearer <token>
```

**Response:**
```json
{
  "request_id": "550e8400-e29b-41d4-a716-446655440000",
  "status": "completed",
  "source": "New York",
  "destination": "Paris",
  "start_date": "2024-06-01",
  "end_date": "2024-06-07",
  "budget": 3000,
  "created_at": "2024-05-15T10:30:00Z",
  "updated_at": "2024-05-15T10:35:00Z",
  "images": [...],
  "places": [...],
  "itinerary": [...]
}
```

---

#### Delete Trip

```http
DELETE /api/trips/{request_id}
Authorization: Bearer <token>
```

**Response:**
```json
{
  "message": "trip deleted successfully"
}
```

---

### AI Chatbot (agentic)

```http
POST /api/chat
Authorization: Bearer <token>
Content-Type: application/json

{
  "message": "Find me a 3-day food trip in Lisbon",
  "conversation_id": "optional-uuid-to-continue",
  "trip_id": "optional-trip-context"
}
```

Streams an **SSE** response while the model runs a tool-use loop (max 10 iterations) over Trequila's MCP
tools — creating trips, geocoding, searching places, fetching images. Uses the caller's default provider
key. The transcript is persisted in `conversations`.

| Event | Payload |
|-------|---------|
| `conversation` | `{"conversation_id":"..."}` — emitted first; pass it back to continue the thread |
| `thinking` | `{"text":"Thinking..."}` |
| `tool_call` | `{"tool":"create_trip","args":{...}}` |
| `tool_result` | `{"tool":"create_trip","result":"<json string>"}` |
| `message` | `{"text":"final assistant reply"}` |
| `error` | `{"text":"..."}` |

`set_api_key`, `remove_api_key`, and `delete_trip` are withheld from the chat loop so a single message (or a
prompt injection) cannot manage credentials or destroy data. They remain available over `/mcp`.

---

### MCP Endpoint

```http
POST /mcp
Authorization: Bearer <token>
```

Model Context Protocol server over the Streamable HTTP transport, so any MCP client (Claude Desktop, IDEs)
can drive Trequila with the user's own account. Exposes trip tools (`create_trip`, `get_trip`,
`get_trip_status`, `list_trips`, `delete_trip`), discovery tools (`geocode`, `search_places`,
`get_destination_images`),
key tools (`list_api_keys`, `set_api_key`, `remove_api_key`), trip resources, and a `plan_trip` prompt. All
tools act as the authenticated user.

---

### Weather Replan

```http
POST /api/trips/replan/{request_id}
Authorization: Bearer <token>
Content-Type: application/json

{ "day_index": 2 }
```

`day_index` is optional; omit it to replan every rainy day (Open-Meteo WMO code ≥ 51). Outdoor activities
are swapped for indoor ones and merged back into the stored itinerary. Requires a generated itinerary.

---

### Route Optimisation

```http
POST /api/trips/optimize/{request_id}
Authorization: Bearer <token>
```

Recomputes the per-day visiting order. A route is generated automatically once a trip's itinerary lands, so
this is only needed to redo one — after editing an itinerary, or for a trip created before routing existed.

Placing the stops means geocoding each activity at roughly one lookup a second, well past the 30-second
request cap, so the work is **enqueued** rather than done inline:

```json
{ "status": "optimizing", "request_id": "550e8400-..." }
```

`202 Accepted` on success, `409` when the trip has no itinerary yet, `404` when it is not yours. Poll
`GET /api/trips/{request_id}` and watch `route.generated_at` change; a route also arrives as a `route` event
on an open SSE stream when it finishes soon enough.

The `route` field is shaped:

```json
{
  "generated_at": "2026-07-30T10:00:00Z",
  "days": [{
    "dayIndex": 0,
    "mode": "walk",
    "ordered": [{"activityRef": 0, "name": "Senso-ji", "lat": 35.7148, "lon": 139.7967, "time": "09:00", "geocoded": false}],
    "legs":    [{"fromRef": 0, "toRef": 1, "distance_m": 1200, "duration_s": 923, "mode": "walk"}],
    "unlocated": [2],
    "total_distance_m": 1200,
    "total_duration_s": 923
  }]
}
```

`mode` is `walk` unless some hop exceeds 2 km. `unlocated` holds the indices of activities with no findable
coordinates ("Dinner somewhere in Shibuya"), kept out of the order rather than dropped silently. Distances
are **straight-line × 1.25** and durations assume 1.3 m/s walking or 7 m/s driving — good enough to order a
day, not turn-by-turn accurate.

---

## 🔴 Server-Sent Events (SSE) - Real-Time Updates

The SSE endpoint provides live updates as trip data is generated by the services. Everything it sends comes from
a durable per-trip event log, so a client that disconnects can reconnect and lose nothing.

### Endpoint

```http
GET /api/trips/stream/{request_id}
Authorization: Bearer <token>
```

### SSE Headers (Set by Server)
```
Content-Type: text/event-stream
Cache-Control: no-cache
Connection: keep-alive
```

### Resuming

Every data event carries an `id:` (an increasing integer). On reconnect, send the last one you saw as the
`Last-Event-ID` header (browsers' `EventSource` and `@microsoft/fetch-event-source` do this automatically), or as
a `lastEventId` query parameter if you cannot set headers. The server replays only what you have not seen. A
`: keepalive` comment is sent every 20 seconds so proxies do not drop the connection; it has no data, so clients
must ignore it.

---

### Event Types

The stream emits the following events:

| Event | Description | When |
|-------|-------------|------|
| `connected` | Connection established | Immediately on connect |
| `images` | Destination images | ~2-5 seconds after trip creation |
| `places` | Points of interest | ~3-7 seconds after trip creation |
| `itinerary` | Full day-by-day plan | ~10-30 seconds after trip creation |
| `route` | Per-day stop ordering | After the itinerary; may arrive after `complete` (use `GET /api/trips/{id}`) |
| `failed` | Generation failed | On LLM auth/credit failure or unrecoverable error |
| `complete` | All data received | Once itinerary and places have both arrived, or 30s after the itinerary if places never do |
| `timeout` | Stream timeout | The stream is capped at 5 minutes |

`failed` carries the stored `error_reason`:
```json
{ "type": "failed", "reason": "provider rejected the API key" }
```

---

### Event Data Formats

#### `connected`
```json
{
  "request_id": "550e8400-e29b-41d4-a716-446655440000",
  "message": "Stream connected"
}
```

#### `images`
```json
{
  "type": "images",
  "images": [
    {
      "url": "https://upload.wikimedia.org/...",
      "alt": "Eiffel Tower",
      "photographer": "Wikipedia"
    }
  ]
}
```

#### `places`
```json
{
  "type": "places",
  "places": [
    {
      "name": "Eiffel Tower",
      "category": "tourism.attraction",
      "address": "Champ de Mars, Paris",
      "lat": 48.8584,
      "lon": 2.2945
    }
  ]
}
```

#### `itinerary`
```json
{
  "type": "itinerary",
  "itinerary": [
    {
      "dayIndex": 1,
      "date": "2024-06-01",
      "summary": "Arrival and Eiffel Tower",
      "activities": [
        {
          "name": "Eiffel Tower Visit",
          "type": "Attraction",
          "time": "10:00",
          "description": "Visit the iconic Eiffel Tower",
          "address": "Champ de Mars, 5 Avenue Anatole France",
          "notes": "Book tickets online to skip the queue",
          "budget": 30
        }
      ]
    }
  ]
}
```

#### `complete`
```json
{
  "message": "All data received"
}
```

---

### Frontend Implementation

#### JavaScript/TypeScript Example

```typescript
interface SSEEvent {
  type: 'images' | 'places' | 'itinerary';
  images?: Image[];
  places?: Place[];
  itinerary?: DayPlan[];
}

function streamTripUpdates(
  requestId: string, 
  token: string,
  callbacks: {
    onImages: (images: Image[]) => void;
    onPlaces: (places: Place[]) => void;
    onItinerary: (itinerary: DayPlan[]) => void;
    onComplete: () => void;
    onError: (error: Error) => void;
  }
): () => void {
  const eventSource = new EventSource(
    `/api/trips/stream/${requestId}`,
    {
      // Note: EventSource doesn't support headers natively
      // Use a polyfill like 'event-source-polyfill' for auth
    }
  );

  // For auth headers, use fetch-event-source or similar:
  // See "With Authentication" section below

  eventSource.addEventListener('connected', (e) => {
    console.log('SSE Connected:', JSON.parse(e.data));
  });

  eventSource.addEventListener('images', (e) => {
    const data = JSON.parse(e.data);
    callbacks.onImages(data.images);
  });

  eventSource.addEventListener('places', (e) => {
    const data = JSON.parse(e.data);
    callbacks.onPlaces(data.places);
  });

  eventSource.addEventListener('itinerary', (e) => {
    const data = JSON.parse(e.data);
    callbacks.onItinerary(data.itinerary);
  });

  eventSource.addEventListener('complete', () => {
    callbacks.onComplete();
    eventSource.close();
  });

  eventSource.onerror = (error) => {
    callbacks.onError(new Error('SSE connection failed'));
    eventSource.close();
  };

  // Return cleanup function
  return () => eventSource.close();
}
```

#### With Authentication (using @microsoft/fetch-event-source)

```bash
npm install @microsoft/fetch-event-source
```

```typescript
import { fetchEventSource } from '@microsoft/fetch-event-source';

async function streamTripWithAuth(requestId: string, token: string) {
  const ctrl = new AbortController();

  await fetchEventSource(`/api/trips/stream/${requestId}`, {
    method: 'GET',
    headers: {
      'Authorization': `Bearer ${token}`,
    },
    signal: ctrl.signal,
    
    onopen(response) {
      if (response.ok) {
        console.log('Connected to SSE');
      } else {
        throw new Error(`Failed to connect: ${response.status}`);
      }
    },
    
    onmessage(event) {
      const data = JSON.parse(event.data);
      
      switch (event.event) {
        case 'connected':
          console.log('Stream connected');
          break;
        case 'images':
          console.log('Received images:', data.images);
          // Update UI with images
          break;
        case 'places':
          console.log('Received places:', data.places);
          // Update UI with places
          break;
        case 'itinerary':
          console.log('Received itinerary:', data.itinerary);
          // Update UI with itinerary
          break;
        case 'complete':
          console.log('All data received');
          ctrl.abort(); // Close connection
          break;
      }
    },
    
    onerror(err) {
      console.error('SSE Error:', err);
      ctrl.abort();
    },
  });

  return ctrl; // Return for manual abort if needed
}
```

#### React Hook Example

```typescript
import { useState, useEffect, useCallback } from 'react';
import { fetchEventSource } from '@microsoft/fetch-event-source';

interface TripStreamState {
  images: Image[] | null;
  places: Place[] | null;
  itinerary: DayPlan[] | null;
  isComplete: boolean;
  isLoading: boolean;
  error: string | null;
}

export function useTripStream(requestId: string | null, token: string) {
  const [state, setState] = useState<TripStreamState>({
    images: null,
    places: null,
    itinerary: null,
    isComplete: false,
    isLoading: false,
    error: null,
  });

  useEffect(() => {
    if (!requestId || !token) return;

    const ctrl = new AbortController();
    setState(prev => ({ ...prev, isLoading: true, error: null }));

    fetchEventSource(`/api/trips/stream/${requestId}`, {
      headers: { 'Authorization': `Bearer ${token}` },
      signal: ctrl.signal,
      
      onmessage(event) {
        const data = JSON.parse(event.data);
        
        switch (event.event) {
          case 'images':
            setState(prev => ({ ...prev, images: data.images }));
            break;
          case 'places':
            setState(prev => ({ ...prev, places: data.places }));
            break;
          case 'itinerary':
            setState(prev => ({ ...prev, itinerary: data.itinerary }));
            break;
          case 'complete':
            setState(prev => ({ ...prev, isComplete: true, isLoading: false }));
            ctrl.abort();
            break;
        }
      },
      
      onerror(err) {
        setState(prev => ({ 
          ...prev, 
          error: 'Connection failed', 
          isLoading: false 
        }));
        ctrl.abort();
      },
    });

    return () => ctrl.abort();
  }, [requestId, token]);

  return state;
}

// Usage in component:
function TripPage({ tripId }: { tripId: string }) {
  const { token } = useAuth();
  const { images, places, itinerary, isLoading, isComplete } = useTripStream(tripId, token);

  return (
    <div>
      {isLoading && <LoadingSpinner />}
      
      {images && <ImageGallery images={images} />}
      {places && <PlacesMap places={places} />}
      {itinerary && <ItineraryTimeline days={itinerary} />}
      
      {isComplete && <p>Trip plan complete! ✅</p>}
    </div>
  );
}
```

---

## Data Types Reference

### Trip Status
| Status | Description |
|--------|-------------|
| `pending` | Trip created, processing started |
| `completed` | All data generated successfully |
| `failed` | Generation failed; `error_reason` explains why (bad key, no credit, provider error, or no itinerary within 10 minutes) |

### Activity Types
| Type | Description |
|------|-------------|
| `Attraction` | Tourist attractions, landmarks |
| `Restaurant` | Dining experiences |
| `Transport` | Travel between locations |
| `Entertainment` | Shows, events, nightlife |
| `Shopping` | Markets, malls, boutiques |
| `Nature` | Parks, beaches, hiking |
| `Culture` | Museums, galleries, historical sites |

### Image Object

Sourced keylessly from Wikipedia REST (hero) and Openverse (gallery); `photographer` carries the
attribution/licence string required by the upstream source.

```typescript
interface Image {
  url: string;
  alt: string;
  photographer: string;
}
```

### Place Object
```typescript
interface Place {
  name: string;
  category: string;
  address: string;
  lat: number;
  lon: number;
}
```

### DayPlan Object
```typescript
interface DayPlan {
  dayIndex: number;
  date: string;        // YYYY-MM-DD
  summary: string;
  activities: Activity[];
}

interface Activity {
  name: string;
  type: string;
  time: string;        // HH:MM
  description: string;
  address: string;
  notes: string;
  budget: number;      // USD
}
```

---

## Error Responses

All errors return JSON with an `error` field:

```json
{
  "error": "error message here"
}
```

### Common Error Codes

| Status | Error | Description |
|--------|-------|-------------|
| 400 | `invalid request body` | Malformed JSON |
| 400 | `missing required fields` | Required fields not provided |
| 401 | `unauthorized` | Missing or invalid token |
| 404 | `trip not found` | Trip doesn't exist or not owned by user |
| 405 | `method not allowed` | Wrong HTTP method |
| 429 | `rate limit exceeded` | Too many requests (10/sec limit) |
| 500 | `database error` | Internal server error |

---

## Rate Limiting

- **Limit:** 10 requests per second per IP
- **Burst:** 20 requests

`X-Forwarded-For` / `X-Real-IP` are only trusted when the connecting peer is a loopback, private, or
link-local address (i.e. a real reverse proxy); otherwise the socket address is used, so clients cannot
spoof their way around the limit.

When rate limited, you'll receive:
```
HTTP 429 Too Many Requests
```

---

## Typical Frontend Flow

```
1. User fills trip form
   ↓
2. POST /api/trips → Get request_id
   ↓
3. Redirect to trip page with request_id
   ↓
4. Connect to SSE: GET /api/trips/stream/{request_id}
   ↓
5. Show loading UI with progress indicators
   ↓
6. As events arrive:
   - images event → Show destination photos
   - places event → Show map with POIs
   - itinerary event → Show day-by-day plan
   ↓
7. complete event → Show full trip, close SSE
   ↓
8. User can revisit: GET /api/trips/{request_id}
   (Returns cached data immediately)
```

---

## CORS

Allowed origins are an exact-match allowlist. They default to `https://trequila.namankhandelwal.dev` and can
be replaced with the comma-separated `CORS_ALLOWED_ORIGINS` env var (e.g.
`http://localhost:3000,https://my-frontend.example`). `*` allows any origin.

Allowed methods: `GET, POST, PUT, DELETE, OPTIONS`
Allowed headers: `Accept, Authorization, Content-Type, X-Requested-With`
Credentials: allowed (required for the `auth_token` cookie).
