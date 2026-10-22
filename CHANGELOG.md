# Changelog

All notable changes are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project uses
[Semantic Versioning](https://semver.org/) once it reaches 1.0.

## [Unreleased]

### Added
- Event-driven foundations: a transport-neutral event bus (NATS JetStream and in-memory adapters), a
  transactional outbox, idempotent consumers and a durable per-trip event log.
- The progress stream resumes with `Last-Event-ID` and no longer misses events published during a reconnect.
- `Idempotency-Key` support on `POST /api/trips`.
- Trips that never receive an itinerary are failed after ten minutes instead of staying pending.
- Structured logging (`LOG_LEVEL`, `LOG_FORMAT`), goose migrations with a `migrate` command, and
  architecture decision records in `docs/adr`.
- CI: linting, race-detector tests against Postgres, and a migration up/down/up check.
- Bring-your-own-key trip planning API with Gemini, Anthropic and OpenAI-compatible providers.
- Live progress over Server-Sent Events: images, places, itinerary and route.
- Keyless enrichment from OpenStreetMap, Wikipedia and Openverse.
- MCP server and agentic chat endpoint.
- Docker Compose stack and Makefile for local development.

### Changed
- The code is split into five services (`api`, `planner`, `discovery`, `router`, `agent`) that communicate only
  through events; `cmd/monolith` just wires them.
- A destination that cannot be geocoded now yields empty places so the trip finishes immediately.
- A database error while looking up a user's key is retried instead of failing the trip as "no key".
- `/metrics` is served only when `METRICS_TOKEN` is set, and then requires it.
- `init.sql` and the dated SQL files are replaced by goose migrations (`make up` runs them).

### Security
- Provider `base_url` values are validated and every connection to them is checked, closing a server-side
  request forgery path to internal services (`ALLOW_PRIVATE_PROVIDER_URLS=true` opts out for self-hosters).
- The key-test endpoint and key handlers no longer return upstream or database error text.
