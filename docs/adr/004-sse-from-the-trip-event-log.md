# 004. Serve progress from a durable trip event log, woken by LISTEN/NOTIFY

Status: accepted

## Context

The progress stream originally read the trip's columns, then subscribed to the broker for live events. An event
published between the read and the subscribe was lost, and the client waited out a timeout. Each stream also held
a broker subscription, so streaming and the message flow were coupled, and a client that dropped its connection
had no way to resume.

## Decision

Everything a client sees is a row in `trip_events`, appended in the same transaction as the change it describes
(`tripevents.Append`). A stream:

1. registers a *doorbell* for its trip with the in-process `Hub`;
2. queries the events after the last one the client has seen (`Last-Event-ID`, or the start);
3. sends them, each with its row ID as the SSE `id:`;
4. waits on the doorbell (or a heartbeat, a grace timer, a hard timeout) and goes back to step 2.

The `Hub` holds one dedicated Postgres connection that `LISTEN`s on a channel; a trigger on `trip_events` issues
`pg_notify` with the trip ID. A doorbell is a one-slot channel: it says "something changed", never what, so a
slow reader cannot lose or block anything. After a reconnect the hub rings every doorbell, because
notifications sent while it was disconnected are gone and readers must re-query.

The stream completes when the itinerary and the places have both arrived, or when places are still missing 30
seconds after the itinerary, so one failed upstream lookup cannot hold a finished itinerary hostage.

## Consequences

- No lost events: the doorbell exists before the first query, so anything committed afterwards still rings it.
- Reconnecting is free and exact, with no duplicates and no gaps.
- Streams no longer touch the broker. Each API instance needs one extra database connection for the listener,
  which matters when connections are scarce (the smallest managed Postgres tiers).
- The log duplicates each result once (it also lives in the `trips` columns). That is the price of an ordered
  history; the columns remain the source for plain `GET` requests.
- `NOTIFY` is per database, not per schema. Doorbells are keyed by trip ID, so unrelated databases or tests
  sharing a server do not interfere.
