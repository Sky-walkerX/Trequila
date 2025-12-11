# Trequila API Documentation

A microservices-based trip planning API with real-time updates via Server-Sent Events (SSE).

## Base URL

- **Development:** `http://localhost` (via Caddy) or `http://localhost:8080` (direct)
- **Production:** `https://your-domain.com`

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
Redirects to Google OAuth. After successful login, redirects to callback with token.

**Callback Response:**
```json
{
  "token": "eyJhbGciOiJIUzI1NiIs...",
  "user": {
    "id": "google-user-id",
    "email": "user@gmail.com",
    "name": "John Doe"
  }
}
```

---

#### Option 4: Generate Token (Development Only)
```http
POST /auth/token
Content-Type: application/json

{
  "user_id": "optional-custom-id",
  "email": "user@example.com",
  "name": "John Doe"
}
```

**Response:**
```json
{
  "token": "eyJhbGciOiJIUzI1NiIs...",
  "user_id": "generated-or-provided-uuid"
}
```

> ⚠️ **Warning:** This endpoint is for development/testing only. It creates tokens without persisting users to the database.

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
  "interests": ["museums", "food", "architecture"]
}
```

**Request Body:**
| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `source` | string | ✅ | Starting location |
| `destination` | string | ✅ | Destination city |
| `start_date` | string | ✅ | Format: `YYYY-MM-DD` |
| `end_date` | string | ✅ | Format: `YYYY-MM-DD` |
| `budget` | int | ❌ | Trip budget in USD |
| `interests` | string[] | ❌ | User interests for personalization |

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

## 🔴 Server-Sent Events (SSE) - Real-Time Updates

The SSE endpoint provides live updates as trip data is generated by the microservices.

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

---

### Event Types

The stream emits the following events:

| Event | Description | When |
|-------|-------------|------|
| `connected` | Connection established | Immediately on connect |
| `images` | Destination images | ~2-5 seconds after trip creation |
| `places` | Points of interest | ~3-7 seconds after trip creation |
| `itinerary` | Full day-by-day plan | ~10-30 seconds after trip creation |
| `complete` | All data received | After itinerary is received |
| `timeout` | Stream timeout (2 min) | If no activity for 2 minutes |

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
      "url": "https://images.pexels.com/...",
      "alt": "Eiffel Tower",
      "photographer": "John Doe",
      "photographer_url": "https://pexels.com/@johndoe"
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
```typescript
interface Image {
  url: string;
  alt: string;
  photographer: string;
  photographer_url: string;
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

The API accepts requests from any origin in development. For production, configure allowed origins in the backend.

Allowed methods: `GET, POST, PUT, DELETE, OPTIONS`
Allowed headers: `Content-Type, Authorization`
