package mcp

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
)

func (s *Server) registerPrompts() {
	planTrip := mcp.NewPrompt("plan_trip",
		mcp.WithPromptDescription("Start planning a new trip"),
		mcp.WithArgument("destination", mcp.ArgumentDescription("Travel destination"), mcp.RequiredArgument()),
		mcp.WithArgument("source", mcp.ArgumentDescription("Starting location"), mcp.RequiredArgument()),
		mcp.WithArgument("dates", mcp.ArgumentDescription("Travel dates (e.g. 2026-07-01 to 2026-07-07)"), mcp.RequiredArgument()),
	)
	s.mcp.AddPrompt(planTrip, s.handlePlanTripPrompt)
}

func (s *Server) handlePlanTripPrompt(ctx context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	args := req.Params.Arguments
	dest := args["destination"]
	src := args["source"]
	dates := args["dates"]

	sysMessage := mcp.PromptMessage{
		Role: mcp.Role("assistant"),
		Content: mcp.TextContent{
			Type: "text",
			Text: "You are Trequila, an expert AI trip planner. The user wants to plan a trip. You should first create the trip using the create_trip tool, then poll get_trip_status until it is completed. Once completed, fetch the full trip details using get_trip and present the itinerary to the user. Present it beautifully using markdown.",
		},
	}

	userMessage := mcp.PromptMessage{
		Role: mcp.Role("user"),
		Content: mcp.TextContent{
			Type: "text",
			Text: fmt.Sprintf("I want to plan a trip from %s to %s for the dates %s. Can you create it and show me the itinerary?", src, dest, dates),
		},
	}

	return &mcp.GetPromptResult{
		Description: "Trip Planning Prompt",
		Messages: []mcp.PromptMessage{
			sysMessage,
			userMessage,
		},
	}, nil
}
