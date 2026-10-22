// Package agent serves the two ways an AI client drives Trequila: the MCP endpoint for
// external clients and the agentic chat endpoint for the product's own UI. Both use
// the same tool registry, and both act as the authenticated user.
package agent

import (
	"database/sql"
	"net/http"

	"github.com/sky-walkerx/trequila/internal/auth"
	"github.com/sky-walkerx/trequila/internal/bus"
	"github.com/sky-walkerx/trequila/internal/chatbot"
	"github.com/sky-walkerx/trequila/internal/geo"
	"github.com/sky-walkerx/trequila/internal/images"
	"github.com/sky-walkerx/trequila/internal/mcp"
	"github.com/sky-walkerx/trequila/internal/providerkeys"
	"github.com/sky-walkerx/trequila/internal/trips"
)

// Deps are the collaborators the service needs.
type Deps struct {
	DB     *sql.DB
	Auth   *auth.AuthService
	Keys   *providerkeys.Store
	Trips  *trips.Store
	Geo    *geo.Service
	Images *images.Service
}

// Service wires MCP and chat.
type Service struct {
	auth *auth.AuthService
	mcp  *mcp.Server
	chat *chatbot.Handler
}

// New creates the service.
func New(d Deps) *Service {
	m := mcp.NewServer(d.DB, d.Trips, d.Keys, d.Geo, d.Images, d.Auth)
	return &Service{
		auth: d.Auth,
		mcp:  m,
		chat: chatbot.NewHandler(chatbot.NewStore(d.DB), d.Keys, m),
	}
}

// Routes registers /mcp and /api/chat behind the normal JWT authentication.
func (s *Service) Routes(mux *http.ServeMux) {
	mux.Handle("/api/chat", s.auth.AuthMiddleware(s.chat))
	mux.Handle("/mcp", s.auth.AuthMiddleware(s.mcp.Handler()))
}

// Subscriptions is empty: the agent only reacts to HTTP requests.
func (s *Service) Subscriptions() []bus.Subscription { return nil }
