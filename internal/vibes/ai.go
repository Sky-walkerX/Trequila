package vibes

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// PlaceInsights holds all the data we extract from the AI
type PlaceInsights struct {
	SafetyScore int    `json:"safety_score"` // 0-100
	SafetyTip   string `json:"safety_tip"`   // "Watch for pickpockets"
	CrowdLevel  string `json:"crowd_level"`  // "Low", "Medium", "High"
	BestTime    string `json:"best_time"`    // "Before 10 AM"
	Duration    string `json:"duration"`     // "1-2 hours"
	PhotoTip    string `json:"photo_tip"`    // "View from Trocadero"
}

// AnalyzePlaceWithAI calls Gemini to get a vibe check
func AnalyzePlaceWithAI(apiKey, placeName, category string) PlaceInsights {
	// Contextual Time (make the AI think about NOW)
	now := time.Now()
	month := now.Month().String()
	hour := now.Hour()
	timeContext := "daytime"
	if hour > 18 || hour < 5 {
		timeContext = "night time"
	}

	// 🧠 The "Super Prompt"
	prompt := fmt.Sprintf(`
		Analyze the location '%s' (Category: %s) for a tourist visiting in %s during %s.
		Provide a structured JSON analysis with:
		1. safety_score: 0 (Dangerous) to 100 (Safe).
		2. safety_tip: A short warning or advice (max 6 words).
		3. crowd_level: Current expected crowds (Low/Medium/High/Extreme).
		4. best_time: Best hour to visit to avoid crowds.
		5. duration: Typical time spent there (e.g. "2 hours").
		6. photo_tip: The best specific spot or angle for a photo.

		Return ONLY raw JSON. No markdown blocks.
		Example: {"safety_score": 90, "safety_tip": "Safe area", "crowd_level": "High", "best_time": "9 AM", "duration": "1 hr", "photo_tip": "From the bridge"}
	`, placeName, category, month, timeContext)

	// Call Gemini (REST API)
	url := "https://generativelanguage.googleapis.com/v1beta/models/gemini-1.5-flash:generateContent?key=" + apiKey

	requestBody := map[string]interface{}{
		"contents": []map[string]interface{}{
			{"parts": []map[string]string{{"text": prompt}}},
		},
		"generationConfig": map[string]interface{}{
			"response_mime_type": "application/json",
		},
	}

	jsonData, _ := json.Marshal(requestBody)
	resp, err := http.Post(url, "application/json", bytes.NewBuffer(jsonData))
	
	// Fallback defaults if AI fails
	fallback := PlaceInsights{
		SafetyScore: 85,
		SafetyTip:   "Standard caution",
		CrowdLevel:  "Unknown",
		BestTime:    "Morning",
		Duration:    "1 hour",
		PhotoTip:    "Front entrance",
	}

	if err != nil {
		return fallback
	}
	defer resp.Body.Close()

	// Parse Response
	var geminiResp struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&geminiResp); err != nil {
		return fallback
	}

	if len(geminiResp.Candidates) == 0 {
		return fallback
	}

	// Extract & Unmarshal the inner JSON
	rawJSON := geminiResp.Candidates[0].Content.Parts[0].Text
	var result PlaceInsights
	if err := json.Unmarshal([]byte(rawJSON), &result); err != nil {
		return fallback
	}

	return result
}