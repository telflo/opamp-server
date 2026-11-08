package middleware

import "net/http"

// Middleware function type
type Middleware func(http.Handler) http.Handler

// ChainMiddleware applies multiple middlewares to a handler
func ChainMiddleware(h http.Handler, middlewares ...Middleware) http.Handler {
	for i := len(middlewares) - 1; i >= 0; i-- {
		h = middlewares[i](h)
	}
	return h
}
