package handlers

import "net/http"

// HealthHandler is the agent listener's liveness probe (GET /health), for
// Kubernetes or a load balancer.
//
// Intentionally minimal: no DB ping, no dependency checks. A failing health
// check causes the platform to restart or stop routing traffic to this
// container, so this endpoint must only fail when the *process itself* is
// unable to serve — not when the database is briefly degraded. Readiness,
// which does ask the database, is the separate /ready (package serve's
// readiness), so liveness and readiness diverge.
//
// Routed outside the agent's middleware (package agent's routes) so
// probes bypass middleware.Authenticator (no bearer token to present) and
// the server span (probes come every few seconds; a span per probe would
// balloon telemetry).
func HealthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write([]byte("ok\n"))
	}
}
