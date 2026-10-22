package mcp

import (
	"context"

	authpkg "github.com/sky-walkerx/trequila/internal/auth"
)

// getUserID extracts the authenticated user ID from context.
//
// Both entry points into the MCP tools run behind auth.AuthMiddleware, which
// stores the user under the auth package's context key:
//   - the /mcp Streamable HTTP transport (wrapped in main.go), and
//   - the in-process chatbot agentic loop (which passes the request context
//     straight through to ExecuteMCPTool).
//
// So we read the same key the auth middleware writes — there is no separate
// MCP auth layer.
func getUserID(ctx context.Context) string {
	return authpkg.GetUserIDFromContext(ctx)
}
