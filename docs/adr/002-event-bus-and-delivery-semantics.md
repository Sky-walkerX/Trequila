# 002. A transport-neutral event bus with at-least-once delivery

Status: accepted

## Context

Services talk through NATS JetStream today and are planned to move to Google Pub/Sub. The original code had six
copy-pasted consumer loops that each handled connecting, fetching, acknowledging and failing slightly
differently, and publishing code scattered through the handlers. Broker clients leaked into business logic,
which would make the planned migration touch every service.

## Decision

`internal/bus` defines the only things services see: an `Event` (ID, type, trace context, JSON payload), a
`Publisher`, a `Handler`, and a `Subscription` (durable name, event type, ack wait, max deliveries). Adapters
implement it: `natsbus` for JetStream, `memory` for tests.

Delivery semantics, identical in every adapter:

- **At least once.** An event may be delivered more than once, so handlers must be idempotent.
- A handler returning `nil` acknowledges the event.
- A handler returning an error wrapped with `bus.Permanent` also acknowledges it, because redelivery cannot fix
  it (a payload that does not decode, a missing key). Everything else, including a panic, is redelivered after a
  short delay, up to the subscription's `MaxDeliver`.
- Event IDs are deterministic wherever one input should produce one output (`trips.itinerary:<trip>`), so the
  broker's de-duplication window and the consumers' processed-event ledger recognise a re-published result.
- Middleware (recover, logging, metrics) wraps every handler the same way.
- Messages without an event envelope, for example from an older version, are skipped rather than looped on.

## Consequences

- Swapping the broker is one new adapter plus wiring; services do not change.
- Business logic is testable on the in-memory bus with real Postgres and no broker.
- An event that exhausts its deliveries is dropped by the adapter. The API therefore fails any trip that has
  produced no itinerary after ten minutes, so a lost event cannot leave a trip pending forever. A dead-letter
  topic arrives with the Pub/Sub adapter.
- Ordering across event types is not guaranteed and is not relied on; handlers tolerate results arriving in any
  order.
