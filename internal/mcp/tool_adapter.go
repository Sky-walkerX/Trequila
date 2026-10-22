package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/sky-walkerx/trequila/internal/ai"
)

// MCPToolDefinitions returns the tools registered on this MCP server in a format the LLMProvider can consume.
func (s *Server) MCPToolDefinitions() []ai.ToolDefinition {
	tools := s.mcp.ListTools()
	var defs []ai.ToolDefinition
	for _, st := range tools {
		var schema map[string]interface{}

		raw, _ := json.Marshal(st.Tool.InputSchema)
		if err := json.Unmarshal(raw, &schema); err != nil {
			continue // a tool whose schema cannot be read cannot be offered to a model
		}

		defs = append(defs, ai.ToolDefinition{
			Name:        st.Tool.Name,
			Description: st.Tool.Description,
			Parameters:  schema,
		})
	}
	return defs
}

// ExecuteMCPTool directly invokes a registered MCP tool internally.
func (s *Server) ExecuteMCPTool(ctx context.Context, toolName string, arguments map[string]interface{}) (string, error) {
	st := s.mcp.GetTool(toolName)
	if st == nil {
		return "", fmt.Errorf("tool %s not found", toolName)
	}

	req := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      toolName,
			Arguments: arguments,
		},
	}

	res, err := st.Handler(ctx, req)
	if err != nil {
		return "", err
	}

	if res.IsError {
		// Try to extract the error text
		for _, content := range res.Content {
			if txt, ok := content.(mcp.TextContent); ok {
				return "", errors.New(txt.Text)
			}
		}
		return "", fmt.Errorf("tool returned error")
	}

	// Extract success output
	for _, content := range res.Content {
		if txt, ok := content.(mcp.TextContent); ok {
			return txt.Text, nil
		}
	}

	return "{}", nil
}
