package mcp

import (
	"database/sql"
	"net/http"

	mcpserver "github.com/mark3labs/mcp-go/server"
	authpkg "github.com/sky-walkerx/trequila/internal/auth"
	"github.com/sky-walkerx/trequila/internal/geo"
	"github.com/sky-walkerx/trequila/internal/images"
	"github.com/sky-walkerx/trequila/internal/providerkeys"
	"github.com/sky-walkerx/trequila/internal/trips"
)

// Server wraps the MCP server with Trequila's service dependencies.
type Server struct {
	db       *sql.DB
	trips    *trips.Store
	keyStore *providerkeys.Store
	geo      *geo.Service
	images   *images.Service
	auth     *authpkg.AuthService
	mcp      *mcpserver.MCPServer
}

// NewServer creates a new Trequila MCP server.
func NewServer(db *sql.DB, tripStore *trips.Store, keyStore *providerkeys.Store, geoSvc *geo.Service, imgSvc *images.Service, authSvc *authpkg.AuthService) *Server {
	s := &Server{
		db:       db,
		trips:    tripStore,
		keyStore: keyStore,
		geo:      geoSvc,
		images:   imgSvc,
		auth:     authSvc,
	}

	mcpSrv := mcpserver.NewMCPServer(
		"Trequila Trip Planner",
		"1.0.0",
		mcpserver.WithToolCapabilities(true),
		mcpserver.WithResourceCapabilities(true, true),
		mcpserver.WithPromptCapabilities(true),
	)

	s.mcp = mcpSrv
	s.registerTools()
	s.registerResources()
	s.registerPrompts()

	return s
}

// Handler returns an http.Handler for the Streamable HTTP transport.
func (s *Server) Handler() http.Handler {
	return mcpserver.NewStreamableHTTPServer(s.mcp)
}

func (s *Server) registerTools() {
	s.registerTripTools()
	s.registerDiscoveryTools()
	s.registerKeyTools()
}
