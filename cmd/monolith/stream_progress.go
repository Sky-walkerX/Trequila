package main

// streamProgress tracks which result stages a trip stream has seen. A trip is
// finished once the itinerary and the places have both arrived; if places never
// do (an Overpass failure), the handler gives up waiting after streamPlacesGrace.
type streamProgress struct {
	Itinerary bool
	Places    bool
}

// Complete reports whether every stage the stream waits for has arrived.
func (p streamProgress) Complete() bool { return p.Itinerary && p.Places }

// GraceDue reports whether the itinerary is in but places are still missing,
// which is when the handler arms the streamPlacesGrace timer.
func (p streamProgress) GraceDue() bool { return p.Itinerary && !p.Places }
