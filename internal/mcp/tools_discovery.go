package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
)

func (s *Server) registerDiscoveryTools() {
	// 1. geocode
	geocode := mcp.NewTool("geocode",
		mcp.WithDescription("Get coordinates for a place name."),
		mcp.WithString("place_name", mcp.Required(), mcp.Description("Name of the place or city")),
	)
	s.mcp.AddTool(geocode, s.handleGeocode)

	// 2. search_places
	searchPlaces := mcp.NewTool("search_places",
		mcp.WithDescription("Find Points of Interest (POIs) near a destination or coordinates. If you pass destination, it will geocode it first. Otherwise provide lat and lon."),
		mcp.WithString("destination", mcp.Description("Optional destination name to geocode first")),
		mcp.WithNumber("lat", mcp.Description("Optional latitude")),
		mcp.WithNumber("lon", mcp.Description("Optional longitude")),
		mcp.WithString("category", mcp.Description("Optional category: hotel, restaurant, attraction. Or empty for all")),
		mcp.WithNumber("limit", mcp.Description("Optional limit (default 20)")),
	)
	s.mcp.AddTool(searchPlaces, s.handleSearchPlaces)

	// 3. get_destination_images
	getImages := mcp.NewTool("get_destination_images",
		mcp.WithDescription("Get images for a destination."),
		mcp.WithString("destination", mcp.Required(), mcp.Description("Destination name")),
	)
	s.mcp.AddTool(getImages, s.handleGetImages)
}

func (s *Server) handleGeocode(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	placeName, _ := req.GetArguments()["place_name"].(string)

	lat, lon, err := s.geo.GetCoordinates(placeName)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to geocode %s: %v", placeName, err)), nil
	}

	res := map[string]interface{}{
		"place": placeName,
		"lat":   lat,
		"lon":   lon,
	}
	resJSON, _ := json.Marshal(res)
	return mcp.NewToolResultText(string(resJSON)), nil
}

func (s *Server) handleSearchPlaces(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	dest, _ := args["destination"].(string)
	latF, _ := args["lat"].(float64)
	lonF, _ := args["lon"].(float64)
	category, _ := args["category"].(string)

	limit := 20
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}

	if dest != "" {
		lt, ln, err := s.geo.GetCoordinates(dest)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Failed to geocode destination: %v", err)), nil
		}
		latF, lonF = lt, ln
	} else if latF == 0 && lonF == 0 {
		return mcp.NewToolResultError("Must provide either destination or lat/lon"), nil
	}

	places, err := s.geo.SearchPlaces(latF, lonF, category, limit)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to search places: %v", err)), nil
	}

	resJSON, _ := json.Marshal(places)
	return mcp.NewToolResultText(string(resJSON)), nil
}

func (s *Server) handleGetImages(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	dest, _ := req.GetArguments()["destination"].(string)
	if dest == "" {
		return mcp.NewToolResultError("destination is required"), nil
	}

	images, err := s.images.GetImages(ctx, dest)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to get images: %v", err)), nil
	}

	resJSON, _ := json.Marshal(images)
	return mcp.NewToolResultText(string(resJSON)), nil
}
