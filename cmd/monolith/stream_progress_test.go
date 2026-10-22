package main

import "testing"

func TestStreamProgressComplete(t *testing.T) {
	cases := []struct {
		name string
		p    streamProgress
		want bool
	}{
		{"nothing yet", streamProgress{}, false},
		{"itinerary only", streamProgress{Itinerary: true}, false},
		{"places only", streamProgress{Places: true}, false},
		{"itinerary and places", streamProgress{Itinerary: true, Places: true}, true},
	}
	for _, c := range cases {
		if got := c.p.Complete(); got != c.want {
			t.Errorf("%s: Complete() = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestStreamProgressGraceDue(t *testing.T) {
	cases := []struct {
		name string
		p    streamProgress
		want bool
	}{
		{"nothing yet", streamProgress{}, false},
		{"itinerary without places", streamProgress{Itinerary: true}, true},
		{"places before itinerary", streamProgress{Places: true}, false},
		{"both arrived", streamProgress{Itinerary: true, Places: true}, false},
	}
	for _, c := range cases {
		if got := c.p.GraceDue(); got != c.want {
			t.Errorf("%s: GraceDue() = %v, want %v", c.name, got, c.want)
		}
	}
}
