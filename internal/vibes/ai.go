package vibes

import (
    "bytes"
    "encoding/json"
    "fmt"
    "log"
    "net/http"
    "time"
)

// PlaceInfo is the input for the AI
type PlaceInfo struct {
    Name     string `json:"name"`
    Category string `json:"category"`
}

// PlaceInsights is the output from the AI
type PlaceInsights struct {
    SafetyScore int    `json:"safety_score"`
    SafetyTip   string `json:"safety_tip"`
    CrowdLevel  string `json:"crowd_level"`
    BestTime    string `json:"best_time"`
    Duration    string `json:"duration"`
    PhotoTip    string `json:"photo_tip"`
}

// BatchAnalyzePlaces sends ALL places to Gemini in one request
func BatchAnalyzePlaces(apiKey string, places []PlaceInfo) map[string]PlaceInsights {
    if len(places) == 0 {
        return nil
    }

    // 1. Construct the Batch Prompt
    placesJSON, _ := json.Marshal(places)
    
    now := time.Now()
    month := now.Month().String()
    timeContext := "daytime"
    if now.Hour() > 18 || now.Hour() < 5 {
        timeContext = "night time"
    }

    prompt := fmt.Sprintf(`
        You are a travel safety and logistics expert.
        Context: Tourist visiting in %s (%s).
        
        Analyze this list of places:
        %s

        Return a JSON Object where the KEY is the exact "name" from the list, and the VALUE is the analysis.
        Strict JSON format. No markdown.
        
        Required fields for each place:
        - safety_score: 0-100 integer
        - safety_tip: max 6 words
        - crowd_level: Low/Medium/High/Extreme
        - best_time: specific hour (e.g. "9 AM")
        - duration: e.g. "1-2 hours"
        - photo_tip: specific angle/spot

        Example Output format:
        {
            "Eiffel Tower": { "safety_score": 85, "safety_tip": "Watch pockets", ... },
            "Louvre": { "safety_score": 90, ... }
        }
    `, month, timeContext, string(placesJSON))

    // 2. Call Gemini
    url := "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent?key=" + apiKey

    requestBody := map[string]interface{}{
        "contents": []map[string]interface{}{
            {"parts": []map[string]string{{"text": prompt}}},
        },
        "generationConfig": map[string]interface{}{
            "response_mime_type": "application/json",
        },
    }

    jsonBody, _ := json.Marshal(requestBody)
    resp, err := http.Post(url, "application/json", bytes.NewBuffer(jsonBody))
    if err != nil {
        log.Printf("Vibes: AI Network Error: %v", err)
        return nil
    }
    defer resp.Body.Close()

    // 3. Parse Response
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
        log.Printf("Vibes: AI Decode Error: %v", err)
        return nil
    }

    if len(geminiResp.Candidates) == 0 || len(geminiResp.Candidates[0].Content.Parts) == 0 {
        return nil
    }

    rawJSON := geminiResp.Candidates[0].Content.Parts[0].Text
    
    // 4. Unmarshal into Map
    results := make(map[string]PlaceInsights)
    if err := json.Unmarshal([]byte(rawJSON), &results); err != nil {
        log.Printf("Vibes: Failed to parse AI JSON result: %v", err)
        return nil
    }

    return results
}