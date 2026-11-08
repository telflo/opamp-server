package middleware

import "net/http"

// SecurityHeadersMiddleware adds common security headers to every answer of
// the agent endpoint, refusals included. There is no CORS header: no
// browser calls the agent endpoint.
func SecurityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-XSS-Protection", "1; mode=block")

		next.ServeHTTP(w, r)
	})
}
