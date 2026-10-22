# Architecture decision records

Short records of the decisions that shape Trequila: the situation, what we chose, and what follows from it.
New records are numbered in sequence and are never rewritten; a decision that changes gets a new record that
supersedes the old one.

| # | Decision |
|---|----------|
| [001](001-service-boundaries-and-data-ownership.md) | Services own their data; one writer per table |
| [002](002-event-bus-and-delivery-semantics.md) | A transport-neutral event bus with at-least-once delivery |
| [003](003-transactional-outbox.md) | Publish events through a transactional outbox |
| [004](004-sse-from-the-trip-event-log.md) | Serve progress from a durable trip event log, woken by LISTEN/NOTIFY |
