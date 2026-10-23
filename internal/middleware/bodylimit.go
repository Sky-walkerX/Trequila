package middleware

import "net/http"

// BodyLimit caps how much of a request body a handler can read. Every JSON endpoint here
// takes a small document, so a large one is a mistake or an attack; the handler's decode
// fails with *http.MaxBytesError and the client sees a 400.
func BodyLimit(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			}
			next.ServeHTTP(w, r)
		})
	}
}
