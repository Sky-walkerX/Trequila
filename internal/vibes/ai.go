package vibes

import (
    "context"
    "encoding/json"
    "fmt"
    "log"
    "strings"
    "time"

    "github.com/google/generative-ai-go/genai"
    "google.golang.org/api/option"
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

// BatchAnalyzePlaces sends ALL places to Gemini in one request using the official SDK
func BatchAnalyzePlaces(ctx context.Context, apiKey string, places []PlaceInfo) map[string]PlaceInsights {
    if len(places) == 0 {
        return nil
    }

    client, err := genai.NewClient(ctx, option.WithAPIKey(apiKey))
    if err != nil {
        log.Printf("Vibes: Failed to create Gemini client: %v", err)
        return nil
    }
    defer client.Close()

    model := client.GenerativeModel("gemini-2.5-flash")
    model.ResponseMIMEType = "application/json"
    model.SetTemperature(0.4)

    // Build the prompt
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

    // Call Gemini with retry logic
    var resp *genai.GenerateContentResponse
    const maxRetries = 3

    for attempt := 0; attempt <= maxRetries; attempt++ {
        genCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
        resp, err = model.GenerateContent(genCtx, genai.Text(prompt))
        cancel()

        if err == nil {
            break
        }

        errMsg := err.Error()
        isRetryable := strings.Contains(errMsg, "429") ||
            strings.Contains(errMsg, "503") ||
            strings.Contains(errMsg, "500") ||
            strings.Contains(errMsg, "resource exhausted") ||
            strings.Contains(errMsg, "unavailable")

        if !isRetryable || attempt == maxRetries {
            log.Printf("Vibes: Gemini API error after %d attempts: %v", attempt+1, err)
            return nil
        }

        backoff := time.Duration(1<<uint(attempt)) * time.Second
        log.Printf("Vibes: retrying in %v (attempt %d/%d): %v", backoff, attempt+1, maxRetries, err)
        select {
        case <-time.After(backoff):
        case <-ctx.Done():
            return nil
        }
    }

    // Parse response
    if len(resp.Candidates) == 0 || len(resp.Candidates[0].Content.Parts) == 0 {
        log.Printf("Vibes: Empty response from Gemini")
        return nil
    }

    var rawJSON string
    for _, part := range resp.Candidates[0].Content.Parts {
        if text, ok := part.(genai.Text); ok {
            rawJSON += string(text)
        }
    }

    results := make(map[string]PlaceInsights)
    if err := json.Unmarshal([]byte(rawJSON), &results); err != nil {
        log.Printf("Vibes: Failed to parse AI JSON result: %v", err)
        return nil
    }

    return results
}