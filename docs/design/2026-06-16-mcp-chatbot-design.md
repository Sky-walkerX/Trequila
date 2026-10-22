# Trequila MCP Server — Design Spec

**Status:** Draft · **Date:** 2026-06-16 · **Author:** Sky-walkerX + Antigravity

---

## 1. Summary

Add a **Model Context Protocol (MCP) server** to Trequila so that any MCP-compatible AI
client — Claude Desktop, Cursor, Windsurf, custom chatbots, or Trequila's own frontend
chatbot — can plan trips conversationally by calling Trequila tools.

The MCP server exposes Trequila's full trip-planning pipeline as structured **tools**,
read-only trip data as **resources**, and canned workflow starters as **prompts**. It
authenticates via the existing JWT system and resolves the user's BYOK LLM key for any
generation work.

A companion **AI chatbot** endpoint in the monolith provides a turnkey agentic experience
for the frontend: the user types natural language, the backend runs an agentic tool-use
loop against the MCP tools, and streams incremental results back.

---

## 2. Goals & Non-Goals

**Goals**

1. Expose Trequila as an MCP server so **any agentic platform** can plan trips.
2. Build a **frontend chatbot** that uses the same MCP tools internally for a seamless
   conversational trip-planning UX.
3. Support both **Streamable HTTP** (remote/web) and **stdio** (local/desktop) transports.
4. Authenticate MCP sessions via existing JWT; reuse BYOK key resolution per-user.
5. Let the AI orchestrate the full flow: create trip → poll status → present results,
   including intermediate "your trip is generating…" feedback.

**Non-Goals**

- Changing the existing NATS async pipeline (MCP tools *call into* it, not replace it).
- Building the frontend chatbot UI (separate repo; we build the backend endpoint).
- Multi-user collaborative MCP sessions (v2).
- MCP *client* implementation (Trequila is only a server).

---

## 3. Architecture Overview

```
┌──────────────────────────────────────────────────────┐
│                   MCP Clients                        │
│  ┌──────────┐  ┌──────────┐  ┌───────────────────┐  │
│  │ Claude   │  │ Cursor / │  │ Trequila Frontend │  │
│  │ Desktop  │  │ Windsurf │  │   Chatbot UI      │  │
│  └────┬─────┘  └────┬─────┘  └────────┬──────────┘  │
│       │stdio         │HTTP            │HTTP          │
└───────┼──────────────┼────────────────┼──────────────┘
        │              │                │
        ▼              ▼                ▼
┌───────────────────────────────────────────────────────┐
│              Trequila Monolith                        │
│                                                       │
│  ┌─────────────────────────────────────────────────┐  │
│  │          MCP Server Layer                       │  │
│  │  (github.com/mark3labs/mcp-go)                  │  │
│  │                                                 │  │
│  │  Transport:                                     │  │
│  │    • Streamable HTTP (/mcp)                     │  │
│  │    • Stdio (separate binary entrypoint)         │  │
│  │                                                 │  │
│  │  Auth Middleware:                               │  │
│  │    JWT from Authorization header/cookie         │  │
│  │    → userID in context for every tool call      │  │
│  │                                                 │  │
│  │  ┌─────────┐ ┌──────────┐ ┌─────────────┐      │  │
│  │  │  Tools  │ │Resources │ │  Prompts    │      │  │
│  │  └────┬────┘ └────┬─────┘ └──────┬──────┘      │  │
│  └───────┼───────────┼──────────────┼──────────────┘  │
│          │           │              │                  │
│          ▼           ▼              ▼                  │
│  ┌─────────────────────────────────────────────────┐  │
│  │        Existing Service Layer                   │  │
│  │  DB │ NATS │ Planner │ Geo │ Images │ Vibes    │  │
│  └─────────────────────────────────────────────────┘  │
│                                                       │
│  ┌─────────────────────────────────────────────────┐  │
│  │     Chatbot Agentic Endpoint                    │  │
│  │  POST /api/chat                                 │  │
│  │  (tool-use loop using LLMProvider + MCP         │  │
│  │   tools internally, SSE streaming)              │  │
│  └─────────────────────────────────────────────────┘  │
└───────────────────────────────────────────────────────┘
```

**Key insight:** The MCP server is a *thin adapter layer*. Every tool handler calls the
same Go functions that the existing HTTP handlers and NATS workers call. No business logic
is duplicated.

---

## 4. MCP Tools

These are the **model-controlled** functions the AI can invoke. Designed for natural
chaining: the AI creates a trip, polls its status, then presents the results.

### 4.1 Trip Lifecycle

| Tool | Description | Key Params | Returns |
|------|-------------|------------|---------|
| `create_trip` | Create a new trip and start async generation | `source`, `destination`, `start_date`, `end_date`, `budget?`, `interests?[]`, `provider?`, `model?` | `{trip_id, status: "pending"}` |
| `get_trip` | Get full trip data including partial results | `trip_id` | Trip object with itinerary, images, places, vibes (whatever is ready) |
| `get_trip_status` | Lightweight status poll (no heavy data) | `trip_id` | `{status, has_itinerary, has_images, has_places, has_vibes, error_reason?}` |
| `list_trips` | List the user's trips | `limit?`, `offset?` | Array of trip summaries |
| `delete_trip` | Delete a trip | `trip_id` | `{deleted: true}` |

### 4.2 Trip Enhancement

| Tool | Description | Key Params | Returns |
|------|-------------|------------|---------|
| `replan_for_weather` | Replan outdoor activities for rainy days | `trip_id` | Updated itinerary |
| `get_budget_tiers` | Generate budget/mid/luxury alternatives | `trip_id` | Three itinerary variants |
| `chat_about_trip` | Ask a question about a specific trip | `trip_id`, `message` | AI response with trip context |
| `share_trip` | Generate a share code for a trip | `trip_id` | `{share_code}` |

### 4.3 Discovery

| Tool | Description | Key Params | Returns |
|------|-------------|------------|---------|
| `search_places` | Find POIs near a destination | `destination` or `lat`+`lon`, `category?` (hotel/restaurant/attraction), `limit?` | Array of places |
| `get_destination_images` | Get images for a destination | `destination` | Array of images with attribution |
| `geocode` | Get coordinates for a place name | `place_name` | `{lat, lon}` |

### 4.4 Configuration

| Tool | Description | Key Params | Returns |
|------|-------------|------------|---------|
| `list_api_keys` | Show configured LLM providers (masked) | — | Array of `{provider, masked_key, model, is_default}` |
| `set_api_key` | Configure an LLM provider key | `provider`, `api_key`, `model`, `base_url?`, `make_default?` | `{saved: true}` |
| `test_api_key` | Validate a provider key | `provider`, `api_key`, `model?`, `base_url?` | `{valid: true}` or error reason |
| `remove_api_key` | Remove a configured provider | `provider` | `{removed: true}` |

### Tool Design Principles

- **Annotations**: Every tool declares `readOnlyHint` (for GET-like tools) and
  `destructiveHint` (for delete). This helps MCP clients decide confirmation UX.
- **Error handling**: Application errors (bad key, trip not found) return
  `{isError: true, content: [{type: "text", text: "..."}]}` — never transport errors.
- **Idempotency**: `create_trip` generates a new UUID each call; `get_trip` is naturally
  idempotent.
- **Progress**: `create_trip` returns immediately with `{trip_id, status: "pending"}`.
  The AI then polls `get_trip_status` to check when results arrive. For the chatbot
  endpoint, we handle this loop internally.

---

## 5. MCP Resources

Read-only data exposed via URI scheme `trequila://`.

| URI Pattern | Description |
|-------------|-------------|
| `trequila:///trips` | List of user's trips (summary) |
| `trequila:///trips/{id}` | Full trip data |
| `trequila:///trips/{id}/itinerary` | Just the itinerary |
| `trequila:///trips/{id}/places` | Just the places |
| `trequila:///trips/{id}/images` | Just the images |
| `trequila:///trips/{id}/vibes` | Just the vibes analysis |
| `trequila:///keys` | Configured providers (masked) |

Resources support **subscriptions**: a client can subscribe to `trequila:///trips/{id}`
and receive updates as each worker completes (itinerary arrives, then images, etc.). This
maps naturally to the existing NATS subscription mechanism.

---

## 6. MCP Prompts

Pre-built prompt templates surfaced to the user as slash commands or quick actions.

| Prompt | Description | Arguments |
|--------|-------------|-----------|
| `plan_trip` | Start planning a new trip | `destination` (required), `source`, `dates`, `budget`, `interests` |
| `explore_destination` | Discover places and images for a destination | `destination` (required) |
| `replan_weather` | Replan a trip for weather | `trip_id` (required) |
| `compare_budgets` | See budget/mid/luxury options | `trip_id` (required) |

Each prompt expands into a system message + user message that naturally leads the AI to
call the right tools. For example, `plan_trip` expands to:

```
You are Trequila, an AI trip planner. The user wants to plan a trip to {destination}.
Use the create_trip tool with the provided details, then poll get_trip_status until
complete, and present the full itinerary with images and places.
```

---

## 7. Authentication

### Remote (Streamable HTTP)

The MCP endpoint at `/mcp` is protected by JWT authentication — the same system used by
all `/api/*` routes.

```
Client → POST /mcp
         Authorization: Bearer <jwt>
         Content-Type: application/json
         
Server → extracts userID from JWT
       → injects into context for all tool handlers
       → tool handlers use userID to resolve BYOK keys, query trips, etc.
```

The `mcp-go` SDK supports HTTP middleware. We wrap the existing `auth.AuthMiddleware`
logic to extract the JWT and inject `userID` into the Go context.

### Local (Stdio)

For Claude Desktop / local use, the stdio transport reads a config file or environment
variable for the JWT token:

```json
// ~/.config/claude/claude_desktop_config.json
{
  "mcpServers": {
    "trequila": {
      "command": "/path/to/trequila-mcp",
      "args": ["--token", "<jwt>"],
      "env": {
        "TREQUILA_API_URL": "https://api.trequila.tech"
      }
    }
  }
}
```

The stdio binary is a thin client that:
1. Accepts MCP messages via stdin/stdout
2. Forwards tool calls to the remote Trequila API (or runs them locally if configured
   with a database connection)

> **Decision point:** For v1, the stdio binary is a **proxy** — it translates MCP stdio
> messages into HTTP calls to the hosted Trequila API. This avoids requiring users to run
> Postgres/NATS/Redis locally just to use Claude Desktop. In v2, a local-first mode could
> connect directly to a local database.

---

## 8. Chatbot Agentic Endpoint

### `POST /api/chat` (SSE streaming)

This is the **turnkey endpoint** for the Trequila frontend chatbot. It runs a full
agentic tool-use loop server-side so the frontend only needs to send natural language
and render streamed results.

```
┌──────────┐     POST /api/chat     ┌──────────────────────────┐
│ Frontend │ ──────────────────────► │ Chatbot Handler          │
│ Chatbot  │     {message,          │                          │
│ UI       │      conversation_id?, │ 1. Load conversation     │
│          │      trip_id?}         │ 2. Build system prompt   │
│          │                        │ 3. Call LLM with tools   │
│          │ ◄────── SSE ────────── │ 4. Execute tool calls    │
│          │  event: thinking       │ 5. Feed results back     │
│          │  event: tool_call      │ 6. Repeat until done     │
│          │  event: tool_result    │ 7. Stream final response │
│          │  event: message        │                          │
│          │  event: trip_update    └──────────────────────────┘
└──────────┘
```

#### Agentic Loop

```go
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
    // 1. Authenticate user
    userID := auth.GetUserIDFromContext(r.Context())
    
    // 2. Resolve user's LLM provider
    providerCfg, _ := s.keyStore.Resolve(ctx, userID, "")
    llm, _ := llm.NewProvider(ctx, providerCfg)
    
    // 3. Build conversation with tool definitions
    tools := s.mcpToolDefinitions() // same tool schemas as MCP
    systemPrompt := buildChatbotSystemPrompt(userID)
    
    // 4. Agentic loop (max 10 iterations)
    for i := 0; i < 10; i++ {
        response := llm.ChatWithTools(ctx, systemPrompt, history, tools)
        
        if response.HasToolCalls() {
            for _, call := range response.ToolCalls {
                // Stream: event: tool_call
                result := s.executeMCPTool(ctx, userID, call)
                // Stream: event: tool_result
                history = append(history, toolResultTurn(call, result))
            }
            continue // let LLM process results
        }
        
        // No more tool calls — stream final message
        // Stream: event: message
        break
    }
}
```

#### SSE Event Types

| Event | Data | Purpose |
|-------|------|---------|
| `thinking` | `{text: "Looking up flights..."}` | Show the AI's reasoning |
| `tool_call` | `{tool: "create_trip", args: {...}}` | Show which tool is being called |
| `tool_result` | `{tool: "create_trip", result: {...}}` | Show tool output |
| `message` | `{text: "I've created your trip!..."}` | Final AI response |
| `trip_update` | `{trip_id, status, ...}` | Real-time trip generation progress |
| `error` | `{text: "..."}` | Error in the agentic loop |

#### Conversation Persistence

Conversations are stored in a new `conversations` table:

```sql
CREATE TABLE conversations (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    trip_id     TEXT,
    title       TEXT DEFAULT 'New Conversation',
    messages    JSONB NOT NULL DEFAULT '[]',
    created_at  TIMESTAMPTZ DEFAULT NOW(),
    updated_at  TIMESTAMPTZ DEFAULT NOW()
);
CREATE INDEX idx_conversations_user ON conversations(user_id);
CREATE INDEX idx_conversations_trip ON conversations(trip_id);
```

Each message in the JSONB array:
```json
{
  "role": "user|assistant|tool",
  "content": "...",
  "tool_calls": [...],
  "tool_call_id": "...",
  "timestamp": "..."
}
```

---

## 9. LLMProvider Enhancement: Tool Use

The existing `LLMProvider` interface needs a new method for tool-calling:

```go
// Addition to internal/ai/provider.go

type ToolDefinition struct {
    Name        string                 `json:"name"`
    Description string                 `json:"description"`
    Parameters  map[string]interface{} `json:"parameters"` // JSON Schema
}

type ToolCall struct {
    ID        string                 `json:"id"`
    Name      string                 `json:"name"`
    Arguments map[string]interface{} `json:"arguments"`
}

type ChatResponse struct {
    Content   string     `json:"content,omitempty"`
    ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// New method on LLMProvider
type LLMProvider interface {
    // ... existing methods ...
    
    // ChatWithTools sends a message with available tool definitions.
    // The response may contain tool_calls that need to be executed.
    ChatWithTools(ctx context.Context, system string, history []ChatTurn,
        tools []ToolDefinition) (ChatResponse, error)
}
```

Each adapter implements this:
- **Gemini**: Uses `genai.Tool` with `FunctionDeclarations`
- **Anthropic**: Uses the `tools` parameter in Messages API
- **OpenAI-compatible**: Uses `tools` in chat completions

---

## 10. Package Structure

```
internal/
├── mcp/
│   ├── server.go          # MCP server setup, tool/resource/prompt registration
│   ├── tools.go           # Tool handler implementations
│   ├── tools_trips.go     # Trip lifecycle tools
│   ├── tools_discovery.go # Places, images, geocode tools
│   ├── tools_keys.go      # API key management tools
│   ├── resources.go       # Resource handlers
│   ├── prompts.go         # Prompt templates
│   ├── auth.go            # MCP auth middleware (JWT extraction)
│   └── server_test.go     # Tests for tool handlers
├── chatbot/
│   ├── handler.go         # POST /api/chat SSE handler
│   ├── agent.go           # Agentic tool-use loop
│   ├── system_prompt.go   # System prompt builder
│   ├── conversation.go    # Conversation persistence
│   └── handler_test.go    # Tests
cmd/
├── monolith/
│   └── main.go            # Adds MCP server + chatbot routes
├── trequila-mcp/
│   └── main.go            # Stdio MCP binary for Claude Desktop
```

---

## 11. Database Migration

```sql
-- migrations/2026-06-16-mcp-chatbot.sql

CREATE TABLE IF NOT EXISTS conversations (
    id          TEXT PRIMARY KEY,
    user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    trip_id     TEXT,
    title       TEXT DEFAULT 'New Conversation',
    messages    JSONB NOT NULL DEFAULT '[]',
    created_at  TIMESTAMPTZ DEFAULT NOW(),
    updated_at  TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX idx_conversations_user ON conversations(user_id);
CREATE INDEX idx_conversations_trip ON conversations(trip_id);
```

---

## 12. Dependency

```
github.com/mark3labs/mcp-go  — MCP server SDK (tools, resources, prompts, transports)
```

This is the dominant Go MCP SDK, actively maintained, supports all three transports
(stdio, SSE, Streamable HTTP), and has a clean API that maps directly to our needs.

---

## 13. Implementation Order

### Phase 1: MCP Server Core (Steps 1–3)
1. **Add `mcp-go` dependency** + create `internal/mcp` package scaffold.
2. **Implement trip lifecycle tools** (`create_trip`, `get_trip`, `get_trip_status`,
   `list_trips`, `delete_trip`) — these are the highest-value tools.
3. **Implement discovery tools** (`search_places`, `get_destination_images`, `geocode`)
   — thin wrappers around existing geo/images services.

### Phase 2: Auth + Transport (Steps 4–5)
4. **MCP auth middleware** — JWT extraction, userID injection into context.
5. **Mount Streamable HTTP transport** on `/mcp` in the monolith + test with
   an MCP client (e.g. `npx @anthropic-ai/mcp-inspector`).

### Phase 3: Enhancement Tools + Resources (Steps 6–7)
6. **Trip enhancement tools** (`replan_for_weather`, `get_budget_tiers`,
   `chat_about_trip`, `share_trip`).
7. **Resources and prompts** — read-only trip data + prompt templates.

### Phase 4: Key Management Tools (Step 8)
8. **API key tools** (`list_api_keys`, `set_api_key`, `test_api_key`,
   `remove_api_key`).

### Phase 5: Chatbot Agentic Endpoint (Steps 9–11)
9. **Extend `LLMProvider` with `ChatWithTools`** method — implement for all
   three adapters (Gemini, Anthropic, OpenAI).
10. **Build the agentic loop** (`internal/chatbot/agent.go`) — tool execution,
    iteration, and SSE streaming.
11. **`POST /api/chat` handler** + conversation persistence + tests.

### Phase 6: Stdio Transport (Step 12)
12. **`cmd/trequila-mcp` binary** — stdio proxy for Claude Desktop.

### Phase 7: Polish (Step 13)
13. **Documentation** — MCP server setup guide in README, Claude Desktop config
    example, chatbot API docs.

---

## 14. Example Conversations

### External MCP Client (Claude Desktop)

```
User: "Plan me a trip from New York to Tokyo, June 20-27, budget $5000"

Claude Desktop:
  → calls create_trip(source="New York", destination="Tokyo", 
                       start_date="2026-06-20", end_date="2026-06-27",
                       budget=5000)
  ← {trip_id: "abc-123", status: "pending"}
  
  → calls get_trip_status(trip_id="abc-123")
  ← {status: "pending", has_itinerary: false, ...}
  
  (waits, polls again)
  → calls get_trip_status(trip_id="abc-123")  
  ← {status: "completed", has_itinerary: true, has_images: true, 
     has_places: true, has_vibes: false}
  
  → calls get_trip(trip_id="abc-123")
  ← {itinerary: [...7 days...], images: [...], places: [...]}

Claude: "Here's your 7-day Tokyo itinerary! Day 1: Arrive at Narita,
         check in to your hotel in Shinjuku..."
```

### Frontend Chatbot (POST /api/chat)

```
User: "Hey, I'm traveling from Colombo to Bali next month, 
       budget around $2000, interested in beaches and temples"

Server (SSE):
  event: thinking
  data: {"text": "Planning a trip from Colombo to Bali..."}
  
  event: tool_call
  data: {"tool": "create_trip", "args": {"source": "Colombo", 
         "destination": "Bali", "start_date": "2026-07-16", 
         "end_date": "2026-07-23", "budget": 2000, 
         "interests": ["beaches", "temples"]}}
  
  event: tool_result
  data: {"tool": "create_trip", "result": {"trip_id": "xyz-789", 
         "status": "pending"}}
  
  event: thinking
  data: {"text": "Trip created! Waiting for the itinerary..."}
  
  event: trip_update
  data: {"trip_id": "xyz-789", "status": "completed", 
         "has_itinerary": true}
  
  event: tool_call
  data: {"tool": "get_trip", "args": {"trip_id": "xyz-789"}}
  
  event: message
  data: {"text": "🌴 Your Bali adventure is ready! Here's your 
         7-day itinerary:\n\n**Day 1 - Arrival**\n..."}
```

---

## 15. Risks & Mitigations

| Risk | Impact | Mitigation |
|------|--------|------------|
| `mcp-go` SDK breaking changes | Build breaks | Pin version; SDK is stable (v1.x) |
| Polling loop for trip status is wasteful | Extra LLM calls in agentic loop | Cap at 10 polls with backoff; chatbot endpoint subscribes to NATS internally instead of polling |
| Tool abuse (mass trip creation) | Resource exhaustion | Rate limiting on MCP endpoint; same limits as API |
| Large tool responses (full itinerary) | Token budget in LLM context | `get_trip_status` (lightweight) vs `get_trip` (heavy) split; truncate if needed |
| Conversation storage growth | DB bloat | TTL on conversations (30 days); limit message count per conversation |
| Claude Desktop auth UX (JWT token) | Friction | Provide a CLI command to generate a long-lived token; document clearly |

---

## 16. Success Criteria

1. ✅ Claude Desktop can plan a trip end-to-end using the stdio MCP binary.
2. ✅ MCP Inspector (`npx @anthropic-ai/mcp-inspector`) connects to `/mcp` and lists all tools.
3. ✅ Frontend chatbot at `POST /api/chat` handles "plan a trip to X" conversationally.
4. ✅ All 14 tools work with proper auth, BYOK key resolution, and error handling.
5. ✅ `go build ./...` and `go test ./...` pass.
