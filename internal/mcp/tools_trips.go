package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sky-walkerx/trequila/internal/providerkeys"
	"github.com/sky-walkerx/trequila/internal/types"
)

func (s *Server) registerTripTools() {
	// 1. create_trip
	createTrip := mcp.NewTool("create_trip",
		mcp.WithDescription("Create a new trip and start async AI generation of itinerary, images, and places. Returns immediately with trip_id and pending status. Use get_trip_status to poll for completion."),
		mcp.WithString("source", mcp.Required(), mcp.Description("Starting city or location")),
		mcp.WithString("destination", mcp.Required(), mcp.Description("Travel destination")),
		mcp.WithString("start_date", mcp.Required(), mcp.Description("Start date in YYYY-MM-DD format")),
		mcp.WithString("end_date", mcp.Required(), mcp.Description("End date in YYYY-MM-DD format")),
		mcp.WithNumber("budget", mcp.Description("Optional budget in USD")),
		mcp.WithString("provider", mcp.Description("Optional provider (gemini, anthropic, openai_compatible)")),
		mcp.WithString("model", mcp.Description("Optional model name")),
	)
	s.mcp.AddTool(createTrip, s.handleCreateTrip)

	// 2. get_trip
	getTrip := mcp.NewTool("get_trip",
		mcp.WithDescription("Get full trip data including partial results (itinerary, images, places)."),
		mcp.WithString("trip_id", mcp.Required(), mcp.Description("The trip ID")),
	)
	s.mcp.AddTool(getTrip, s.handleGetTrip)

	// 3. get_trip_status
	getTripStatus := mcp.NewTool("get_trip_status",
		mcp.WithDescription("Lightweight status poll for a trip."),
		mcp.WithString("trip_id", mcp.Required(), mcp.Description("The trip ID")),
	)
	s.mcp.AddTool(getTripStatus, s.handleGetTripStatus)

	// 4. list_trips
	listTrips := mcp.NewTool("list_trips",
		mcp.WithDescription("List the user's trips."),
		mcp.WithNumber("limit", mcp.Description("Optional limit (default 20)")),
		mcp.WithNumber("offset", mcp.Description("Optional offset (default 0)")),
	)
	s.mcp.AddTool(listTrips, s.handleListTrips)

	// 5. delete_trip
	deleteTrip := mcp.NewTool("delete_trip",
		mcp.WithDescription("Delete a trip."),
		mcp.WithString("trip_id", mcp.Required(), mcp.Description("The trip ID")),
	)
	s.mcp.AddTool(deleteTrip, s.handleDeleteTrip)
}

func (s *Server) handleCreateTrip(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	userID := getUserID(ctx)
	if userID == "" {
		return mcp.NewToolResultError("unauthorized: no user in context"), nil
	}

	args := req.GetArguments()
	source, _ := args["source"].(string)
	destination, _ := args["destination"].(string)
	startDate, _ := args["start_date"].(string)
	endDate, _ := args["end_date"].(string)
	budgetF, _ := args["budget"].(float64)
	budget := int(budgetF)
	provider, _ := args["provider"].(string)
	model, _ := args["model"].(string)

	sd, err := time.Parse("2006-01-02", startDate)
	if err != nil {
		return mcp.NewToolResultError("invalid start_date format, expected YYYY-MM-DD"), nil
	}
	ed, err := time.Parse("2006-01-02", endDate)
	if err != nil {
		return mcp.NewToolResultError("invalid end_date format, expected YYYY-MM-DD"), nil
	}
	if ed.Before(sd) {
		return mcp.NewToolResultError("end_date must be after start_date"), nil
	}

	days := int(ed.Sub(sd).Hours()/24) + 1

	cfg, err := s.keyStore.Resolve(ctx, userID, provider)
	if err != nil {
		if errors.Is(err, providerkeys.ErrNoKey) {
			return mcp.NewToolResultError("Please configure an API key for the AI provider first"), nil
		}
		return mcp.NewToolResultError("Failed to resolve API key"), nil
	}
	if provider == "" {
		provider = string(cfg.Kind)
	}
	if model == "" {
		model = cfg.Model
	}

	requestID := uuid.NewString()

	_, err = s.db.Exec(`
		INSERT INTO trips (request_id, user_id, source, destination, start_date, end_date, budget, status, provider, model)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'pending', $8, $9)`,
		requestID, userID, source, destination, startDate, endDate, budget, provider, model,
	)
	if err != nil {
		log.Printf("DB Insert error: %v", err)
		return mcp.NewToolResultError("Database error when creating trip"), nil
	}

	event := types.TripEvent{
		Type: "TripRequested",
		Data: types.TripRequest{
			RequestID:   requestID,
			UserID:      userID,
			Source:      source,
			Destination: destination,
			StartDate:   startDate,
			EndDate:     endDate,
			Days:        days,
			Budget:      budget,
			Interests:   []string{}, // optional
			Provider:    provider,
			Model:       model,
			CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		},
	}

	data, _ := json.Marshal(event)
	pubCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	_, err = s.js.Publish(pubCtx, "trips.requested", data, jetstream.WithMsgID(requestID))
	if err != nil {
		log.Printf("NATS Publish error: %v", err)
		return mcp.NewToolResultError("Failed to queue trip for generation"), nil
	}

	res := map[string]interface{}{
		"trip_id": requestID,
		"status":  "pending",
	}
	resJSON, _ := json.Marshal(res)
	return mcp.NewToolResultText(string(resJSON)), nil
}

func (s *Server) handleGetTrip(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	userID := getUserID(ctx)
	if userID == "" {
		return mcp.NewToolResultError("unauthorized: no user in context"), nil
	}

	tripID, _ := req.GetArguments()["trip_id"].(string)
	if tripID == "" {
		return mcp.NewToolResultError("trip_id is required"), nil
	}

	var trip struct {
		RequestID   string
		Source      string
		Destination string
		StartDate   string
		EndDate     string
		Budget      int
		Status      string
		Itinerary   []byte
		Images      []byte
		Places      []byte
	}
	var errStr sql.NullString
	err := s.db.QueryRow(`
		SELECT request_id, source, destination, start_date, end_date, budget, status,
		       error_reason, itinerary, images, places
		FROM trips WHERE request_id = $1 AND user_id = $2`, tripID, userID).Scan(
		&trip.RequestID, &trip.Source, &trip.Destination, &trip.StartDate, &trip.EndDate, &trip.Budget,
		&trip.Status, &errStr, &trip.Itinerary, &trip.Images, &trip.Places,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return mcp.NewToolResultError("Trip not found or access denied"), nil
		}
		return mcp.NewToolResultError("Database error"), nil
	}

	res := map[string]interface{}{
		"trip_id":     trip.RequestID,
		"source":      trip.Source,
		"destination": trip.Destination,
		"start_date":  trip.StartDate,
		"end_date":    trip.EndDate,
		"budget":      trip.Budget,
		"status":      trip.Status,
	}

	if errStr.Valid && errStr.String != "" {
		res["error_reason"] = errStr.String
	}
	if len(trip.Itinerary) > 0 {
		var it []interface{}
		json.Unmarshal(trip.Itinerary, &it)
		res["itinerary"] = it
	}
	if len(trip.Images) > 0 {
		var img []interface{}
		json.Unmarshal(trip.Images, &img)
		res["images"] = img
	}
	if len(trip.Places) > 0 {
		var pl []interface{}
		json.Unmarshal(trip.Places, &pl)
		res["places"] = pl
	}

	resJSON, _ := json.Marshal(res)
	return mcp.NewToolResultText(string(resJSON)), nil
}

func (s *Server) handleGetTripStatus(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	userID := getUserID(ctx)
	if userID == "" {
		return mcp.NewToolResultError("unauthorized"), nil
	}

	tripID, _ := req.GetArguments()["trip_id"].(string)

	var status string
	var errStr sql.NullString
	var hasItin, hasImg, hasPl bool

	err := s.db.QueryRow(`
		SELECT status, error_reason,
			(itinerary IS NOT NULL) as has_itin,
			(images IS NOT NULL) as has_img,
			(places IS NOT NULL) as has_pl
		FROM trips WHERE request_id = $1 AND user_id = $2`, tripID, userID).Scan(
		&status, &errStr, &hasItin, &hasImg, &hasPl,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return mcp.NewToolResultError("Trip not found"), nil
		}
		return mcp.NewToolResultError("Database error"), nil
	}

	res := map[string]interface{}{
		"status":        status,
		"has_itinerary": hasItin,
		"has_images":    hasImg,
		"has_places":    hasPl,
	}
	if errStr.Valid && errStr.String != "" {
		res["error_reason"] = errStr.String
	}

	resJSON, _ := json.Marshal(res)
	return mcp.NewToolResultText(string(resJSON)), nil
}

func (s *Server) handleListTrips(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	userID := getUserID(ctx)
	if userID == "" {
		return mcp.NewToolResultError("unauthorized"), nil
	}

	limit := 20
	if l, ok := req.GetArguments()["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}
	offset := 0
	if o, ok := req.GetArguments()["offset"].(float64); ok && o >= 0 {
		offset = int(o)
	}

	rows, err := s.db.Query(`
		SELECT request_id, source, destination, start_date, end_date, budget, status, created_at
		FROM trips WHERE user_id = $1 ORDER BY created_at DESC LIMIT $2 OFFSET $3`,
		userID, limit, offset,
	)
	if err != nil {
		return mcp.NewToolResultError("Database error"), nil
	}
	defer rows.Close()

	var trips []map[string]interface{}
	for rows.Next() {
		var reqID, src, dest, sdate, edate, status string
		var budget int
		var createdAt time.Time
		if err := rows.Scan(&reqID, &src, &dest, &sdate, &edate, &budget, &status, &createdAt); err != nil {
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
			"created_at":  createdAt.Format(time.RFC3339),
		})
	}
	if trips == nil {
		trips = []map[string]interface{}{}
	}

	resJSON, _ := json.Marshal(trips)
	return mcp.NewToolResultText(string(resJSON)), nil
}

func (s *Server) handleDeleteTrip(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	userID := getUserID(ctx)
	if userID == "" {
		return mcp.NewToolResultError("unauthorized"), nil
	}

	tripID, _ := req.GetArguments()["trip_id"].(string)
	res, err := s.db.Exec(`DELETE FROM trips WHERE request_id = $1 AND user_id = $2`, tripID, userID)
	if err != nil {
		return mcp.NewToolResultError("Database error"), nil
	}
	aff, _ := res.RowsAffected()
	if aff == 0 {
		return mcp.NewToolResultError("Trip not found or already deleted"), nil
	}

	resJSON, _ := json.Marshal(map[string]bool{"deleted": true})
	return mcp.NewToolResultText(string(resJSON)), nil
}
