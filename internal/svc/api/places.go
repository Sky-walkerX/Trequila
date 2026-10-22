package api

import (
	"encoding/json"

	"github.com/sky-walkerx/trequila/internal/geo"
)

func flattenPlaces(raw []byte) []geo.Place {
	if len(raw) == 0 {
		return nil
	}

	var flat []geo.Place
	if err := json.Unmarshal(raw, &flat); err == nil {
		return flat
	}

	var grouped map[string][]geo.Place
	if err := json.Unmarshal(raw, &grouped); err != nil {
		return nil
	}
	var out []geo.Place
	for _, group := range grouped {
		out = append(out, group...)
	}
	return out
}
