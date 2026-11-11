package agent

import (
	"io"
	"net/http"

	"github.com/telflo/telflo-opamp-server/handlers"
	"github.com/telflo/telflo-opamp-server/middleware"
)

// The agent listener: the port collectors poll, apart from the management
// API's, behind its own load balancer. Its routes, and nothing else:
//
//	POST /opamp/api/v1   the poll: middleware.AgentStack around
//	                     handlers.AgentHandler
//	GET  /health         liveness: 200 "ok", no database
//	GET  /ready          readiness: the process's (Options.Ready; package
//	                     serve's answers 200 once serving while the database
//	                     answers, 503 while starting, once draining, or when
//	                     it doesn't)
//	anything else        404, with no log line and no span: nothing of the
//	                     path or the user agent is written down
//
// No CORS: no browser calls it.

// The agent listener's fixed paths.
const (
	healthPath = "/health"
	readyPath  = "/ready"
)

// routes is the agent listener's handler: poll on POST
// /opamp/api/v1, /health, ready on /ready, and 404 for anything else. Paths
// match exactly (no cleaning, no redirect).
func routes(poll, ready http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probe := r.Method == http.MethodGet || r.Method == http.MethodHead
		switch {
		case r.URL.Path == middleware.AgentRoute && r.Method == http.MethodPost:
			poll.ServeHTTP(w, r)
		case r.URL.Path == healthPath && probe:
			handlers.HealthHandler(w, r)
		case r.URL.Path == readyPath && probe:
			ready.ServeHTTP(w, r)
		default:
			notFound(w)
		}
	})
}

// notFound answers anything the agent listener doesn't serve: 404,
// fixed text, written down nowhere.
func notFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, "Not Found\n")
}
