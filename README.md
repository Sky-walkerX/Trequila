# Trequila

Trequila is an open-source, bring-your-own-key (BYOK) AI-powered trip-planning API. It generates itineraries, destination images and nearby places, and streams the results back via Server-Sent Events.

## Quickstart

Users can supply their own LLM API keys via the Trequila frontend. It supports Google Gemini, Anthropic Claude, and OpenAI-compatible endpoints.

## Self-Hosting Guide

Trequila uses a single-binary architecture with background workers that communicate via NATS JetStream.

### Prerequisites
- Go 1.25+
- PostgreSQL
- NATS Server with JetStream enabled
- Redis

### Environment Variables

Required variables:
- `DATABASE_URL`: PostgreSQL connection string.
- `REDIS_URL`: Redis connection string.
- `NATS_URL`: NATS connection string.
- `JWT_SECRET`: Used for signing JWTs. Must be at least 32 bytes long.
- `ENCRYPTION_KEY`: A 32-byte base64 encoded string used for encrypting user API keys at rest.

Optional variables:
- `CORS_ALLOWED_ORIGINS`: Comma-separated browser origins allowed to call the API. Defaults to `https://trequila.namankhandelwal.dev`.
- `FRONTEND_URL`: Base URL the Google OAuth callback redirects back to. Defaults to `https://trequila.namankhandelwal.dev`.
- `GOOGLE_CLIENT_ID` / `GOOGLE_CLIENT_SECRET` / `GOOGLE_REDIRECT_URL`: enable Google OAuth login.
- `OSM_NOMINATIM_URL`: Nominatim geocoding API URL.
- `OSM_OVERPASS_URL`: Overpass API URL for POIs.
- `WIKIPEDIA_API_URL`: Wikipedia REST API URL for hero images.
- `OPENVERSE_API_URL`: Openverse API URL for galleries.
- `HTTP_USER_AGENT`: User-Agent string for HTTP requests to open data APIs.

### Setup
1. Generate an `ENCRYPTION_KEY`: `head -c 32 /dev/urandom | base64`
2. Set up your database schema with `psql "$DATABASE_URL" -f init.sql`.
3. Build the binary: `go build -o monolith ./cmd/monolith`
4. Run: `./monolith`

## Architecture

Trequila runs all HTTP handlers and background workers in one binary. They communicate asynchronously via an external NATS JetStream stream.
- **Planner Worker:** Calls the user's configured LLM for itinerary generation.
- **Places Worker:** Keyless enrichment using OSM Nominatim and Overpass.
- **Images Worker:** Keyless enrichment using Wikipedia REST and Openverse.

## License
Apache 2.0
