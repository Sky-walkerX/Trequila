package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sky-walkerx/trequila/internal/auth"
	"github.com/sky-walkerx/trequila/internal/llm"
	"github.com/sky-walkerx/trequila/internal/providerkeys"
	"github.com/sky-walkerx/trequila/internal/types"
)

func (s *Service) handleWeatherReplan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	userID := auth.GetUserIDFromContext(r.Context())
	if userID == "" {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}

	tripID := strings.TrimPrefix(r.URL.Path, "/api/trips/replan/")
	if tripID == "" {
		http.Error(w, `{"error":"trip id required"}`, http.StatusBadRequest)
		return
	}

	// Get existing trip data
	var destination, provider, model string
	var itineraryJSON []byte

	err := s.db.QueryRow(`
		SELECT destination, itinerary, COALESCE(provider, ''), COALESCE(model, '')
		FROM trips WHERE request_id = $1 AND user_id = $2 AND itinerary IS NOT NULL`,
		tripID, userID,
	).Scan(&destination, &itineraryJSON, &provider, &model)

	if err == sql.ErrNoRows {
		http.Error(w, `{"error":"trip not found"}`, http.StatusNotFound)
		return
	} else if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}

	var itinerary []types.DayPlan
	if len(itineraryJSON) > 0 {
		json.Unmarshal(itineraryJSON, &itinerary)
	}

	if len(itinerary) == 0 {
		http.Error(w, `{"error":"no itinerary generated yet"}`, http.StatusBadRequest)
		return
	}

	// Parse optional request body for specific day index
	var req struct {
		DayIndex *int `json:"day_index"` // nil = replan all rainy days
	}
	json.NewDecoder(r.Body).Decode(&req)

	// Fetch weather forecast for the destination
	// Use geocoding to get lat/lon (we'll use a simple approach)
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	// Identify rainy days using Open-Meteo forecast
	forecastURL := fmt.Sprintf(
		"https://api.open-meteo.com/v1/forecast?latitude=0&longitude=0&daily=weathercode,temperature_2m_max&timezone=auto&forecast_days=%d",
		len(itinerary),
	)
	// We need lat/lon — fetch from geocoding
	geoURL := fmt.Sprintf("https://geocoding-api.open-meteo.com/v1/search?name=%s&count=1", strings.ReplaceAll(destination, " ", "+"))
	geoResp, err := http.Get(geoURL)
	if err == nil {
		defer geoResp.Body.Close()
		var geoData struct {
			Results []struct {
				Lat float64 `json:"latitude"`
				Lon float64 `json:"longitude"`
			} `json:"results"`
		}
		if json.NewDecoder(geoResp.Body).Decode(&geoData) == nil && len(geoData.Results) > 0 {
			forecastURL = fmt.Sprintf(
				"https://api.open-meteo.com/v1/forecast?latitude=%.4f&longitude=%.4f&daily=weathercode,temperature_2m_max&timezone=auto&forecast_days=%d",
				geoData.Results[0].Lat, geoData.Results[0].Lon, len(itinerary),
			)
		}
	}

	// Fetch weather forecast
	type weatherForecast struct {
		Daily struct {
			WeatherCode []int     `json:"weathercode"`
			MaxTemp     []float64 `json:"temperature_2m_max"`
		} `json:"daily"`
	}

	var forecast weatherForecast
	wResp, err := http.Get(forecastURL)
	if err == nil {
		defer wResp.Body.Close()
		json.NewDecoder(wResp.Body).Decode(&forecast)
	}

	// Determine which days need replanning
	rainyDays := []int{} // indices into itinerary
	weatherInfo := map[int]string{}
	for i, code := range forecast.Daily.WeatherCode {
		if i >= len(itinerary) {
			break
		}
		if code >= 51 { // Rain/Storm codes per WMO
			rainyDays = append(rainyDays, i)
			temp := 0.0
			if i < len(forecast.Daily.MaxTemp) {
				temp = forecast.Daily.MaxTemp[i]
			}
			emoji := "🌧️"
			if code >= 95 {
				emoji = "⛈️"
			}
			weatherInfo[i] = fmt.Sprintf("%.0f°C %s", temp, emoji)
		}
	}

	// If specific day requested, only replan that day
	if req.DayIndex != nil {
		found := false
		for _, d := range rainyDays {
			if d == *req.DayIndex {
				found = true
				break
			}
		}
		if !found {
			// User wants to force-replan a specific day regardless of weather
			rainyDays = []int{*req.DayIndex}
			weatherInfo[*req.DayIndex] = "User requested replan"
		} else {
			rainyDays = []int{*req.DayIndex}
		}
	}

	if len(rainyDays) == 0 {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"message":    "No rainy days detected — itinerary looks good!",
			"rainy_days": []int{},
			"replanned":  false,
			"itinerary":  itinerary,
		})
		return
	}

	cfg, err := s.keys.Resolve(ctx, userID, provider)
	if err != nil {
		if errors.Is(err, providerkeys.ErrNoKey) {
			http.Error(w, `{"error":"add an API key in Settings"}`, http.StatusBadRequest)
		} else {
			http.Error(w, `{"error":"failed to resolve API key"}`, http.StatusInternalServerError)
		}
		return
	}
	if model != "" {
		cfg.Model = model
	}

	llmProvider, err := llm.NewProvider(ctx, cfg)
	if err != nil {
		http.Error(w, `{"error":"AI service unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	defer llmProvider.Close()

	// Build replan prompt
	daysToReplan := []types.DayPlan{}
	for _, idx := range rainyDays {
		if idx < len(itinerary) {
			daysToReplan = append(daysToReplan, itinerary[idx])
		}
	}
	daysJSON, _ := json.Marshal(daysToReplan)

	prompt := fmt.Sprintf(`You are an expert travel planner. The following days of a trip to %s have BAD WEATHER (rain/storms).
Replace outdoor activities with indoor alternatives while keeping the same time structure and budget.

Days to replan (JSON):
%s

Weather conditions:
%v

Requirements:
- Keep the same dayIndex, date, and time slots
- Replace outdoor activities (Nature, Attraction with outdoor venues) with indoor alternatives (museums, galleries, indoor markets, cooking classes, spa, etc.)
- Keep restaurants and indoor activities as-is
- Maintain similar budget levels
- Return ONLY a JSON array of the replanned days with the same structure

Return a JSON array of replanned day objects.`, destination, string(daysJSON), weatherInfo)

	jsonStr, err := llmProvider.GenerateJSON(ctx, prompt)
	if err != nil {
		s.log.Warn("replan: provider error", "trip_id", tripID, "error", err)
		http.Error(w, `{"error":"failed to generate alternatives"}`, http.StatusInternalServerError)
		return
	}

	jsonStr = strings.TrimSpace(jsonStr)
	if start := strings.Index(jsonStr, "["); start != -1 {
		if end := strings.LastIndex(jsonStr, "]"); end > start {
			jsonStr = jsonStr[start : end+1]
		}
	}

	var replannedDays []types.DayPlan
	if err := json.Unmarshal([]byte(jsonStr), &replannedDays); err != nil {
		s.log.Warn("replan: could not parse the model output", "trip_id", tripID, "error", err)
		http.Error(w, `{"error":"failed to parse replanned itinerary"}`, http.StatusInternalServerError)
		return
	}

	// Merge replanned days into original itinerary
	replannedMap := map[int]types.DayPlan{}
	for _, day := range replannedDays {
		replannedMap[day.DayIndex] = day
	}
	for i := range itinerary {
		if replacement, ok := replannedMap[itinerary[i].DayIndex]; ok {
			itinerary[i] = replacement
		}
	}

	// Save updated itinerary
	updatedJSON, _ := json.Marshal(itinerary)
	s.db.Exec(`UPDATE trips SET itinerary = $1, updated_at = NOW() WHERE request_id = $2`,
		string(updatedJSON), tripID)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message":    fmt.Sprintf("Replanned %d day(s) with indoor alternatives", len(rainyDays)),
		"rainy_days": rainyDays,
		"weather":    weatherInfo,
		"replanned":  true,
		"itinerary":  itinerary,
	})
}
