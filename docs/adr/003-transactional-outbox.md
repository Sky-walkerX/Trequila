# 003. Publish events through a transactional outbox

Status: accepted

## Context

Creating a trip writes a row and must announce it. The original code inserted the row and then published to
the broker as two separate steps. A crash or a broker outage between them left a trip that was stored but never
planned (a "pending" row nothing would ever pick up), and the reverse order would announce trips that were never
stored. The same problem exists wherever a consumer stores a result and emits a follow-up event.

## Decision

State changes that must produce an event write the event to an `outbox` table in the same database transaction
(`outbox.Add`). A `Relay` publishes pending rows afterwards:

- `Kick` runs right after the commit, with a two-second budget, so the normal path adds almost no latency. A
  failure is logged and never fails the user's request.
- `Run` sweeps every few seconds as the safety net, picking rows with `FOR UPDATE SKIP LOCKED` so several
  instances can run at once without publishing a row twice. Failed rows keep their error and attempt count.
- Published rows are purged after seven days; the backlog size is exported as a metric.

On the consuming side, `inbox.Handle` records each processed event ID in the same transaction as the
consumer's own writes, so a redelivered event is acknowledged without being applied again.

## Consequences

- Either both the state change and its event exist or neither does; a crash at any point is repaired by the next
  sweep.
- Delivery is at least once end to end, never exactly once. The event ID is the de-duplication key at the
  broker, in the outbox (unique) and in the inbox.
- The database is now part of the publishing path: if Postgres is down nothing is published, which is correct,
  because nothing could have been committed either.
- A little extra write load and a table that needs pruning, both handled by the relay.
