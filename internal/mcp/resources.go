package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

func (s *Server) registerResources() {
	// trips list
	s.mcp.AddResource(
		mcp.NewResource("trequila:///trips", "User Trips", mcp.WithResourceDescription("List of all user trips"), mcp.WithMIMEType("application/json")),
		s.handleResourceTrips,
	)

	// Single trip details resource template
	s.mcp.AddResourceTemplate(
		mcp.NewResourceTemplate("trequila:///trips/{id}", "Trip Details", mcp.WithTemplateDescription("Full details of a specific trip"), mcp.WithTemplateMIMEType("application/json")),
		s.handleResourceTripDetails,
	)
}

func (s *Server) handleResourceTrips(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	userID := getUserID(ctx)
	if userID == "" {
		return nil, fmt.Errorf("unauthorized")
	}

	rows, err := s.db.Query(`
		SELECT request_id, source, destination, start_date, end_date, budget, status
		FROM trips WHERE user_id = $1 ORDER BY created_at DESC LIMIT 50`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var trips []map[string]interface{}
	for rows.Next() {
		var reqID, src, dest, sdate, edate, status string
		var budget int
		if err := rows.Scan(&reqID, &src, &dest, &sdate, &edate, &budget, &status); err != nil {
			continue
		}
		trips = append(trips, map[string]interface{}{
			"trip_id":     reqID,
			"source":      src,
			"destination": dest,
			"start_date":  sdate,
			"end_date":    edate,
			"budget":      budget,
			"status":      status,
		})
	}

	resJSON, _ := json.Marshal(trips)
	return []mcp.ResourceContents{
		mcp.TextResourceContents{
			URI:      req.Params.URI,
			MIMEType: "application/json",
			Text:     string(resJSON),
		},
	}, nil
}

func (s *Server) handleResourceTripDetails(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	userID := getUserID(ctx)
	if userID == "" {
		return nil, fmt.Errorf("unauthorized")
	}

	parts := strings.Split(req.Params.URI, "/")
	if len(parts) < 5 {
		return nil, fmt.Errorf("invalid uri format")
	}
	tripID := parts[4]

	var trip struct {
		RequestID   string
		Source      string
		Destination string
		Status      string
		Itinerary   []byte
	}
	err := s.db.QueryRow(`
		SELECT request_id, source, destination, status, itinerary
		FROM trips WHERE request_id = $1 AND user_id = $2`, tripID, userID).Scan(
		&trip.RequestID, &trip.Source, &trip.Destination, &trip.Status, &trip.Itinerary,
	)
	if err != nil {
		return nil, err
	}

	res := map[string]interface{}{
		"trip_id":     trip.RequestID,
		"source":      trip.Source,
		"destination": trip.Destination,
		"status":      trip.Status,
	}
	if len(trip.Itinerary) > 0 {
		var it []interface{}
		json.Unmarshal(trip.Itinerary, &it)
		res["itinerary"] = it
	}

	resJSON, _ := json.Marshal(res)
	return []mcp.ResourceContents{
		mcp.TextResourceContents{
			URI:      req.Params.URI,
			MIMEType: "application/json",
			Text:     string(resJSON),
		},
	}, nil
}
