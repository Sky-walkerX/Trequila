# 001. Services own their data; one writer per table

Status: accepted

## Context

Trequila started as one binary whose HTTP handlers, workers and database writes lived together in a single
file. The pieces already communicated only through a message stream, but nothing stated which piece was allowed
to change which data, so any of them could write to any table. We want to run the pieces as separate
deployables later (the roadmap targets separate Cloud Run services) without a rewrite, and we want a design a
reviewer can read as a set of clear boundaries.

## Decision

The code is split into five services under `internal/svc`: **api** (public HTTP, accounts, trips, keys, the
progress stream), **planner** (turns a request into an itinerary with the user's LLM key), **discovery** (places
and images), **router** (orders each day's stops) and **agent** (MCP and chat). Each exposes `Routes(mux)` and
`Subscriptions()`; `cmd/monolith` only wires them together.

Rules:

1. A service never imports another service. They share event contracts (`internal/events`) and low-level
   packages, nothing else.
2. Each table has exactly one writing service. Today the API owns `users`, `trips`, `provider_keys`,
   `trip_members`, `trip_events`, `outbox`, `processed_events` and `idempotency_keys`; the agent owns
   `conversations`. Workers that need a
   result stored publish an event and the owner stores it. As workers gain tables of their own (itinerary
   versions, candidates), they own those.
3. Cross-service reads go through events or an explicit API call, not through shared tables. Any exception is
   written down here when it is made.

## Consequences

- The monolith is a deployment choice, not an architecture: splitting a service out means giving it its own
  `main` and a bus connection.
- Result events carry the data the owner needs (the route request carries the trip's places because the router
  has no database), which makes events larger but keeps workers stateless.
- Referential rules between services' data (a result for a deleted trip) become runtime checks: the API drops
  results for trips that no longer exist.
- It is more ceremony than one package would need. That is deliberate; the boundaries are the point.
