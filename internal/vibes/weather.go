package vibes

import (
	"encoding/json"
	"fmt"
	"net/http"
)

type weatherResponse struct {
	Daily struct {
		WeatherCode []int     `json:"weathercode"`
		MaxTemp     []float64 `json:"temperature_2m_max"`
	} `json:"daily"`
}

// GetWeather returns a short summary (e.g. "24°C ☀️")
func GetWeather(lat, lon float64) (string, string) {
	// Request forecast for "today" (1 day) using auto timezone
	url := fmt.Sprintf(
		"https://api.open-meteo.com/v1/forecast?latitude=%.4f&longitude=%.4f&daily=weathercode,temperature_2m_max&timezone=auto&forecast_days=1",
		lat, lon,
	)

	resp, err := http.Get(url)
	if err != nil {
		return "Unknown", ""
	}
	defer resp.Body.Close()

	var w weatherResponse
	if err := json.NewDecoder(resp.Body).Decode(&w); err != nil || len(w.Daily.WeatherCode) == 0 {
		return "Unknown", ""
	}

	temp := w.Daily.MaxTemp[0]
	code := w.Daily.WeatherCode[0]
	
	vibe := fmt.Sprintf("%.0f°C", temp)
	
	// WMO Weather interpretation codes
	// 0=Clear, 1-3=Cloudy, 51+=Rain
	if code >= 95 {
		return vibe + " ⛈️", "Stormy"
	} else if code >= 51 {
		return vibe + " 🌧️", "Rainy"
	} else if code <= 3 {
		return vibe + " ☀️", "Clear"
	}
	return vibe + " ☁️", "Cloudy"
}