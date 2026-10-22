package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/sky-walkerx/trequila/internal/providerkeys"
)

func (s *Server) registerKeyTools() {
	// 1. list_api_keys
	listKeys := mcp.NewTool("list_api_keys",
		mcp.WithDescription("Show configured LLM providers (masked keys)."),
	)
	s.mcp.AddTool(listKeys, s.handleListKeys)

	// 2. set_api_key
	setKey := mcp.NewTool("set_api_key",
		mcp.WithDescription("Configure an LLM provider key."),
		mcp.WithString("provider", mcp.Required(), mcp.Description("Provider name (gemini, anthropic, openai_compatible)")),
		mcp.WithString("api_key", mcp.Required(), mcp.Description("The API key")),
		mcp.WithString("model", mcp.Required(), mcp.Description("Default model name")),
		mcp.WithString("base_url", mcp.Description("Optional base URL for OpenAI compatible providers")),
		mcp.WithBoolean("make_default", mcp.Description("Set this provider as default")),
	)
	s.mcp.AddTool(setKey, s.handleSetKey)

	// 3. remove_api_key
	removeKey := mcp.NewTool("remove_api_key",
		mcp.WithDescription("Remove a configured provider API key."),
		mcp.WithString("provider", mcp.Required(), mcp.Description("Provider name")),
	)
	s.mcp.AddTool(removeKey, s.handleRemoveKey)
}

func (s *Server) handleListKeys(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	userID := getUserID(ctx)
	if userID == "" {
		return mcp.NewToolResultError("unauthorized"), nil
	}

	keys, err := s.keyStore.List(ctx, userID)
	if err != nil {
		return mcp.NewToolResultError("Failed to list keys: " + err.Error()), nil
	}

	resJSON, _ := json.Marshal(keys)
	return mcp.NewToolResultText(string(resJSON)), nil
}

func (s *Server) handleSetKey(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	userID := getUserID(ctx)
	if userID == "" {
		return mcp.NewToolResultError("unauthorized"), nil
	}

	args := req.GetArguments()
	provider, _ := args["provider"].(string)
	apiKey, _ := args["api_key"].(string)
	model, _ := args["model"].(string)
	baseURL, _ := args["base_url"].(string)
	makeDefault, _ := args["make_default"].(bool)

	err := s.keyStore.Upsert(ctx, userID, provider, apiKey, baseURL, model, makeDefault)
	if err != nil {
		if errors.Is(err, providerkeys.ErrInvalidBaseURL) {
			return mcp.NewToolResultError(err.Error()), nil
		}
		slog.Error("mcp set_api_key failed", "user_id", userID, "error", err)
		return mcp.NewToolResultError("Failed to save key"), nil
	}

	resJSON, _ := json.Marshal(map[string]bool{"saved": true})
	return mcp.NewToolResultText(string(resJSON)), nil
}

func (s *Server) handleRemoveKey(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	userID := getUserID(ctx)
	if userID == "" {
		return mcp.NewToolResultError("unauthorized"), nil
	}

	provider, _ := req.GetArguments()["provider"].(string)

	err := s.keyStore.Delete(ctx, userID, provider)
	if err != nil {
		return mcp.NewToolResultError("Failed to delete key: " + err.Error()), nil
	}

	resJSON, _ := json.Marshal(map[string]bool{"removed": true})
	return mcp.NewToolResultText(string(resJSON)), nil
}
