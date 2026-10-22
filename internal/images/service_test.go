package images

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetImages(t *testing.T) {
	wikiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{
			"title": "Paris",
			"originalimage": {
				"source": "https://example.com/paris.jpg"
			}
		}`))
	}))
	defer wikiServer.Close()

	openverseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{
			"results": [
				{
					"url": "https://example.com/paris2.jpg",
					"creator": "John Doe",
					"title": "Eiffel Tower",
					"license": "by"
				}
			]
		}`))
	}))
	defer openverseServer.Close()

	svc := NewService("")
	svc.wikipediaURL = wikiServer.URL
	svc.openverseURL = openverseServer.URL
	svc.client = wikiServer.Client()

	images, err := svc.GetImages(context.Background(), "Paris")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if len(images) != 2 {
		t.Fatalf("expected 2 images, got %d", len(images))
	}

	if images[0].URL != "https://example.com/paris.jpg" {
		t.Errorf("expected wiki image url, got %s", images[0].URL)
	}

	if images[1].Photographer != "John Doe (CC by)" {
		t.Errorf("expected Openverse CC string, got %s", images[1].Photographer)
	}
}
