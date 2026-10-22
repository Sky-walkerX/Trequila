package chatbot

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sky-walkerx/trequila/internal/ai"
	"github.com/sky-walkerx/trequila/internal/auth"
	"github.com/sky-walkerx/trequila/internal/llm"
	trequilamcp "github.com/sky-walkerx/trequila/internal/mcp"
	"github.com/sky-walkerx/trequila/internal/providerkeys"
)

type ChatRequest struct {
	Message        string `json:"message"`
	ConversationID string `json:"conversation_id"`
	TripID         string `json:"trip_id"`
}

type SSEWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func (s *SSEWriter) Send(event string, data interface{}) {
	b, _ := json.Marshal(data)
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, string(b))
	s.f.Flush()
}

// toolError renders an error message as a JSON object that is always valid to
// feed back to the model, regardless of quotes or newlines in the message.
func toolError(msg string) string {
	b, _ := json.Marshal(map[string]string{"error": msg})
	return string(b)
}

type Handler struct {
	store     *Store
	keys      *providerkeys.Store
	mcpServer *trequilamcp.Server
}

func NewHandler(store *Store, keys *providerkeys.Store, mcpServer *trequilamcp.Server) *Handler {
	return &Handler{
		store:     store,
		keys:      keys,
		mcpServer: mcpServer,
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	userID := auth.GetUserIDFromContext(r.Context())
	if userID == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	var req ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Setup SSE
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	sse := &SSEWriter{w: w, f: flusher}

	// 1. Resolve user's LLM provider
	providerCfg, err := h.keys.Resolve(r.Context(), userID, "")
	if err != nil {
		sse.Send("error", map[string]string{"text": "Failed to resolve AI provider key: " + err.Error()})
		return
	}

	llmProv, err := llm.NewProvider(r.Context(), providerCfg)
	if err != nil {
		sse.Send("error", map[string]string{"text": "Failed to initialize AI provider: " + err.Error()})
		return
	}
	defer llmProv.Close()

	// 2. Load conversation
	var conv *Conversation
	if req.ConversationID != "" {
		conv, err = h.store.GetConversation(r.Context(), req.ConversationID, userID)
		if err != nil {
			sse.Send("error", map[string]string{"text": "Failed to load conversation"})
			return
		}
	}
	if conv == nil {
		conv = &Conversation{
			ID:     uuid.NewString(),
			UserID: userID,
			TripID: req.TripID,
			Title:  "Chat",
		}
	}

	// Emit the id up front so the client can resume this conversation even if
	// the stream is interrupted before the final save.
	sse.Send("conversation", map[string]string{"conversation_id": conv.ID})

	// Append user message
	conv.Messages = append(conv.Messages, ai.ChatTurn{
		Role:    "user",
		Content: req.Message,
	})

	// The request context already carries the authenticated user (set by
	// auth.AuthMiddleware); the MCP tools read it via auth.GetUserIDFromContext.
	ctx := r.Context()

	// 3. Agentic Loop
	h.runAgentLoop(ctx, llmProv, conv, sse)

	// Persist on a detached context: if the client hung up mid-stream, r.Context()
	// is already cancelled and the transcript would be lost.
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := h.store.SaveConversation(saveCtx, conv); err != nil {
		log.Printf("chatbot: failed to save conversation %s: %v", conv.ID, err)
	}
}

// chatExcludedTools are MCP tools withheld from the chat agent loop. Letting the
// LLM manage API keys or delete trips off a single user message (or a prompt
// injection) is too dangerous. They remain available on the /mcp transport,
// which is driven by an explicit client rather than free-form chat.
var chatExcludedTools = map[string]bool{
	"set_api_key":    true,
	"remove_api_key": true,
	"delete_trip":    true,
}

func (h *Handler) runAgentLoop(ctx context.Context, llmProv ai.LLMProvider, conv *Conversation, sse *SSEWriter) {
	all := h.mcpServer.MCPToolDefinitions()
	tools := make([]ai.ToolDefinition, 0, len(all))
	for _, t := range all {
		if chatExcludedTools[t.Name] {
			continue
		}
		tools = append(tools, t)
	}
	systemPrompt := BuildSystemPrompt()

	for i := 0; i < 10; i++ {
		sse.Send("thinking", map[string]string{"text": "Thinking..."})

		resp, err := llmProv.ChatWithTools(ctx, systemPrompt, conv.Messages, tools)
		if err != nil {
			sse.Send("error", map[string]string{"text": "AI error: " + err.Error()})
			return
		}

		if len(resp.ToolCalls) > 0 {
			// Record assistant's tool call turn
			conv.Messages = append(conv.Messages, ai.ChatTurn{
				Role:      "assistant",
				Content:   resp.Content,
				ToolCalls: resp.ToolCalls,
			})

			for _, tc := range resp.ToolCalls {
				sse.Send("tool_call", map[string]interface{}{
					"tool": tc.Name,
					"args": tc.Arguments,
				})

				var resStr string
				if chatExcludedTools[tc.Name] {
					resStr = toolError("tool not available in chat")
				} else if out, err := h.mcpServer.ExecuteMCPTool(ctx, tc.Name, tc.Arguments); err != nil {
					resStr = toolError(err.Error())
				} else {
					resStr = out
				}

				sse.Send("tool_result", map[string]interface{}{
					"tool":   tc.Name,
					"result": resStr, // Just sending the JSON string
				})

				// Record tool result. Name is required so Gemini (which has no
				// tool-call IDs) can map the result back to its function call;
				// Anthropic/OpenAI use ToolCallID.
				conv.Messages = append(conv.Messages, ai.ChatTurn{
					Role:       "tool",
					Content:    resStr,
					ToolCallID: tc.ID,
					Name:       tc.Name,
				})
			}
			// Loop again so LLM can process tool results
			continue
		}

		// No more tool calls - done
		conv.Messages = append(conv.Messages, ai.ChatTurn{
			Role:    "assistant",
			Content: resp.Content,
		})

		content := strings.TrimSpace(resp.Content)
		if content != "" {
			sse.Send("message", map[string]string{"text": content})
		}
		break
	}
}
