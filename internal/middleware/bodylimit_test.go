package middleware

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBodyLimitStopsReadingPastTheCap(t *testing.T) {
	var readErr error
	h := BodyLimit(1024)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, readErr = io.Copy(io.Discard, r.Body)
	}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/", strings.NewReader(strings.Repeat("x", 2048))))
	var tooBig *http.MaxBytesError
	if !errors.As(readErr, &tooBig) {
		t.Fatalf("reading 2 KiB past a 1 KiB cap gave %v, want *http.MaxBytesError", readErr)
	}

	readErr = nil
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/", strings.NewReader("small")))
	if readErr != nil {
		t.Errorf("a small body failed: %v", readErr)
	}
}
