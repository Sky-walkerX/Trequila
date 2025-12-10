# Trequila Architecture Diagrams

## System Overview

```
┌──────────────────────────────────────────────────────────────────────┐
│                         TREQUILA SYSTEM                               │
└──────────────────────────────────────────────────────────────────────┘

┌─────────────┐         ┌──────────────────────┐         ┌─────────────┐
│   Client    │         │    NATS Server       │         │   Planner   │
│  (Browser)  │         │   + JetStream        │         │   Service   │
│             │         │   (Docker)           │         │             │
└──────┬──────┘         └──────────┬───────────┘         └──────┬──────┘
       │                           │                            │
       │ HTTP POST                 │                            │
       │ /trips                    │                            │
       ▼                           │                            │
┌─────────────┐                    │                            │
│     API     │                    │                            │
│   Service   │◄───────────────────┤                            │
│  :8080      │                    │                            │
└──────┬──────┘                    │                            │
       │                           │                            │
       │ Publish                   │                            │
       │ "trips.requested"         │                            │
       └──────────────────────────►│                            │
                                   │                            │
                                   │ Consume                    │
                                   │ "trips.requested"          │
                                   └───────────────────────────►│
                                   │                            │
                                   │ Publish                    │
                                   │ "trips.itinerary"          │
                                   │◄───────────────────────────┘
                                   │
```

## Detailed Message Flow

```
┌─────────────────────────────────────────────────────────────────────┐
│ STEP 1: User Request                                                │
└─────────────────────────────────────────────────────────────────────┘

User ──POST /trips──► API Service
                      └─► Generate UUID: "abc-123"
                      └─► Create Event:
                          {
                            "type": "TripRequested",
                            "data": {
                              "request_id": "abc-123",
                              "source": "NYC",
                              "destination": "Tokyo",
                              ...
                            }
                          }

┌─────────────────────────────────────────────────────────────────────┐
│ STEP 2: Publish to JetStream                                       │
└─────────────────────────────────────────────────────────────────────┘

API Service ──Publish──► NATS JetStream
                         Subject: "trips.requested"
                         MsgID: "abc-123"
                         
                         JetStream:
                         ┌─────────────────────────┐
                         │ TRIPS Stream            │
                         │ ┌─────────────────────┐ │
                         │ │ Seq: 1              │ │
                         │ │ Subject: trips.req  │ │
                         │ │ Data: {...}         │ │
                         │ │ MsgID: abc-123      │ │
                         │ │ Timestamp: ...      │ │
                         │ └─────────────────────┘ │
                         └─────────────────────────┘

API Service ◄──ACK─────── JetStream
            (Sequence: 1)

API ──Response──► User
  {"request_id": "abc-123", "seq": 1, "status": "processing"}

┌─────────────────────────────────────────────────────────────────────┐
│ STEP 3: Planner Consumes Message                                   │
└─────────────────────────────────────────────────────────────────────┘

Planner Service
    │
    │ consumer.Fetch(1)
    ▼
┌─────────────────────────┐
│ Durable Consumer        │
│ "planner-durable"       │
│                         │
│ Current Position: 0     │
│ Next Message: Seq 1     │
└────────┬────────────────┘
         │
         │ Fetch
         ▼
┌─────────────────────────┐
│ TRIPS Stream            │
│ Returns: Message #1     │
└────────┬────────────────┘
         │
         ▼
Planner receives message
    │
    ├─► Parse JSON
    ├─► Extract request_id: "abc-123"
    ├─► Log: "Processing request abc-123"
    └─► Process trip (AI logic - 2s sleep)

┌─────────────────────────────────────────────────────────────────────┐
│ STEP 4: Planner Publishes Result                                   │
└─────────────────────────────────────────────────────────────────────┘

Planner Service
    │
    │ Create Event:
    │ {
    │   "type": "ItineraryGenerated",
    │   "data": {
    │     "request_id": "abc-123",
    │     "steps": ["Day 1...", "Day 2...", "Day 3..."],
    │     "generated_at": "2025-12-10T10:30:00Z"
    │   }
    │ }
    │
    │ Publish
    ▼
┌─────────────────────────┐
│ TRIPS Stream            │
│ Subject: trips.itinerary│
│ MsgID: abc-123          │
│ Seq: 2                  │
└─────────────────────────┘

Planner ◄──ACK────── JetStream
        (Sequence: 2)

┌─────────────────────────────────────────────────────────────────────┐
│ STEP 5: Acknowledge Original Message                               │
└─────────────────────────────────────────────────────────────────────┘

Planner Service
    │
    │ msg.Ack()
    ▼
┌─────────────────────────┐
│ Durable Consumer        │
│ "planner-durable"       │
│                         │
│ Current Position: 1 ✅  │
│ Message #1: Processed   │
└─────────────────────────┘
```

## Stream State Over Time

```
Time T0: Stream Empty
┌─────────────────────┐
│ TRIPS Stream        │
│                     │
│ Messages: 0         │
│ Bytes: 0            │
└─────────────────────┘


Time T1: User sends request → API publishes
┌─────────────────────────────────┐
│ TRIPS Stream                    │
│                                 │
│ [1] trips.requested (abc-123)   │
│     Status: Pending             │
│     Consumer: Not delivered     │
└─────────────────────────────────┘


Time T2: Planner fetches (not acked yet)
┌─────────────────────────────────┐
│ TRIPS Stream                    │
│                                 │
│ [1] trips.requested (abc-123)   │
│     Status: In Progress         │
│     Consumer: planner-durable   │
│     Last Delivered: T2          │
└─────────────────────────────────┘


Time T3: Planner publishes itinerary
┌─────────────────────────────────┐
│ TRIPS Stream                    │
│                                 │
│ [1] trips.requested (abc-123)   │
│     Status: In Progress         │
│                                 │
│ [2] trips.itinerary (abc-123)   │
│     Status: Pending             │
└─────────────────────────────────┘


Time T4: Planner acks message #1
┌─────────────────────────────────┐
│ TRIPS Stream                    │
│                                 │
│ [1] trips.requested (abc-123)   │
│     Status: Acked ✅            │
│     AckTime: T4                 │
│                                 │
│ [2] trips.itinerary (abc-123)   │
│     Status: Pending             │
└─────────────────────────────────┘
```

## Consumer State Machine

```
                    ┌─────────────┐
                    │   WAITING   │
                    │  for fetch  │
                    └──────┬──────┘
                           │
                           │ Fetch(1)
                           ▼
                    ┌─────────────┐
                    │  FETCHING   │
                    │   message   │
                    └──────┬──────┘
                           │
                    ┌──────┴──────┐
                    │             │
            Message │             │ No Message
            Found   │             │ (timeout)
                    ▼             ▼
            ┌─────────────┐  ┌─────────────┐
            │ PROCESSING  │  │   WAITING   │
            │   message   │  │  (continue) │
            └──────┬──────┘  └─────────────┘
                   │
        ┌──────────┼──────────┐
        │          │          │
    Success    Failure    Crash
        │          │          │
        ▼          ▼          ▼
    ┌────┐    ┌────┐    ┌──────────┐
    │Ack │    │Nak │    │No Ack    │
    └─┬──┘    └─┬──┘    │Redelivery│
      │         │       │after 30s │
      │         │       └──────────┘
      │         │
      │         │ Immediate
      │         │ Redelivery
      │         │
      └─────────┴────────► WAITING
```

## Fault Tolerance Scenarios

### Scenario 1: Planner Crashes Before Ack

```
T0: Message delivered to Planner
    Consumer Position: 0 → 1 (in-progress)
    AckWait Timer: Started (30s)

T1: Planner crashes 💥
    Message: Still marked "in-progress"
    Consumer: Disconnected

T2 (30s later): AckWait expires
    JetStream: No ack received
    Action: Redelivery

T3: Planner restarts
    Consumer: Reconnects
    Receives: Same message (redelivery #2)
    Processes: Successfully
    Acks: ✅

T4: Message marked as processed
    Consumer Position: 1
```

### Scenario 2: Duplicate API Request

```
T0: User clicks "Submit" → Request sent
    API: Publishes with MsgID "abc-123"
    JetStream: Accepts (Seq 1)

T1: Browser timeout, user clicks again
    API: Publishes with MsgID "abc-123" (same!)
    JetStream: Rejects (duplicate within 2min window)
    API: Still returns success (idempotent)

Result: Only ONE message in stream
        Planner processes ONCE
```

### Scenario 3: Planner Processes but Publish Fails

```
T0: Planner receives trip request
    Consumer Position: In-progress

T1: Processes successfully
    Itinerary: Generated

T2: Attempts to publish itinerary
    Network Error ❌
    Does NOT ack original message

T3 (30s later): Original message redelivered
    Planner: Processes again
    Itinerary: Regenerated

T4: Publish succeeds
    MsgID: abc-123 (same as T2 attempt)
    JetStream: Accepts (first successful publish)
    Planner: Acks original message ✅

Result: Exactly-once itinerary publication
        (even though processing happened twice)
```

## Scaling: Multiple Planner Instances

```
        ┌──────────────────────────┐
        │   TRIPS Stream           │
        │   trips.requested        │
        └────────────┬─────────────┘
                     │
        ┌────────────┴─────────────┐
        │                          │
        │  Durable Consumer        │
        │  "planner-durable"       │
        │                          │
        └────┬────────┬─────────┬──┘
             │        │         │
     ┌───────┘        │         └────────┐
     │                │                  │
     ▼                ▼                  ▼
┌─────────┐      ┌─────────┐       ┌─────────┐
│Planner 1│      │Planner 2│       │Planner 3│
│Instance │      │Instance │       │Instance │
└─────────┘      └─────────┘       └─────────┘

Distribution:
- Message #1 → Planner 1
- Message #2 → Planner 2
- Message #3 → Planner 3
- Message #4 → Planner 1
- ...

JetStream ensures:
✅ Each message delivered to ONE instance
✅ Load balanced automatically
✅ If instance crashes, message goes to another
```

## Event-Driven Architecture

```
Future Extensions:

                    ┌──────────────┐
                    │ TRIPS Stream │
                    └───────┬──────┘
                            │
        ┌──────────┬────────┼────────┬──────────┐
        │          │        │        │          │
        ▼          ▼        ▼        ▼          ▼
    ┌────────┐ ┌──────┐ ┌──────┐ ┌──────┐ ┌────────┐
    │Planner │ │Email │ │SMS   │ │Slack │ │Analytics│
    │Service │ │Send  │ │Send  │ │Bot   │ │Service │
    └────────┘ └──────┘ └──────┘ └──────┘ └────────┘
        │
        │ Publishes
        ▼
    trips.itinerary
        │
    ┌───┴────┬─────────┬──────────┐
    │        │         │          │
    ▼        ▼         ▼          ▼
┌──────┐ ┌──────┐ ┌────────┐ ┌──────┐
│Email │ │SMS   │ │Booking │ │UI    │
│Send  │ │Send  │ │Service │ │Update│
└──────┘ └──────┘ └────────┘ └──────┘

All services are:
✅ Independent
✅ Scalable
✅ Resilient
✅ Testable
```
