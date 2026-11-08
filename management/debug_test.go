package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/telflo/opamp-server/internal/diagnostics"
	"github.com/telflo/opamp-server/middleware"
)

// debugGet sends a GET of target through the server, with authorization
// ("" for none).
func debugGet(s *Server, method, target, authorization string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, nil)
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	recorder := httptest.NewRecorder()
	s.Handler().ServeHTTP(recorder, request)
	return recorder
}

// The profiles and the admission view are on the management listener,
// behind the secret: without it (or with a wrong one, or with it as Basic
// credentials) every one answers the 401 envelope; with it, a goroutine
// dump, a heap profile, the index and the admission view's JSON. cmdline
// isn't served, and another method on a diagnostic path is the API's 404.
func TestTheDiagnosticRoutesNeedTheManagementSecret(t *testing.T) {
	s, _, log := apiServer(t)
	snapshot := middleware.AdmissionSnapshot{Enabled: true, MaxInFlight: 100, InFlight: 7,
		OldestInFlightAge: 4*time.Minute + 3500*time.Millisecond, OverStaleThreshold: 2, StaleThreshold: 10 * time.Second, ShedTotal: 9}
	s.admission = func() middleware.AdmissionSnapshot { return snapshot }
	targets := []string{diagnostics.PprofPrefix, diagnostics.PprofPrefix + "goroutine?debug=2", diagnostics.PprofPrefix + "heap?debug=1", diagnostics.AdmissionPath}
	for _, target := range targets {
		for _, authorization := range []string{"", "Bearer a-wrong-secret-that-is-long-enough-0123", "Basic " + testSecret} {
			if rec := debugGet(s, http.MethodGet, target, authorization); rec.Code != http.StatusUnauthorized || rec.Body.String() != unauthorized {
				t.Fatalf("GET %s with %q: %d %s", target, authorization, rec.Code, rec.Body.String())
			}
		}
	}

	bearer := "Bearer " + testSecret
	if rec := debugGet(s, http.MethodGet, diagnostics.PprofPrefix, bearer); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "goroutine") {
		t.Fatalf("the index: %d", rec.Code)
	}
	rec := debugGet(s, http.MethodGet, diagnostics.PprofPrefix+"goroutine?debug=2", bearer)
	if body := rec.Body.String(); rec.Code != http.StatusOK || !strings.HasPrefix(body, "goroutine ") || strings.Contains(body, "<html") {
		t.Fatalf("a goroutine dump: %d %.200q", rec.Code, body)
	}
	rec = debugGet(s, http.MethodGet, diagnostics.PprofPrefix+"heap?debug=1", bearer)
	if body := rec.Body.String(); rec.Code != http.StatusOK || !strings.HasPrefix(body, "heap profile:") {
		t.Fatalf("a heap profile: %d %.200q", rec.Code, body)
	}
	if rec := debugGet(s, http.MethodGet, diagnostics.PprofPrefix+"cmdline", bearer); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"not_found"`) {
		t.Fatalf("cmdline: %d %s", rec.Code, rec.Body.String())
	}
	if rec := debugGet(s, http.MethodPost, diagnostics.AdmissionPath, bearer); rec.Code != http.StatusNotFound {
		t.Fatalf("POST the admission view: %d", rec.Code)
	}

	rec = debugGet(s, http.MethodGet, diagnostics.AdmissionPath, bearer)
	var got map[string]any
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") ||
		rec.Header().Get("Cache-Control") != "no-store" || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
		t.Fatalf("the admission view: %d %v %s", rec.Code, rec.Header(), rec.Body.String())
	}
	for key, want := range map[string]any{
		"enabled": true, "max_in_flight": float64(100), "in_flight": float64(7), "oldest_in_flight_age": "4m3.5s",
		"in_flight_over_stale_threshold": float64(2), "stale_threshold": "10s", "shed_total": float64(9),
	} {
		if got[key] != want {
			t.Errorf("%s = %v, want %v", key, got[key], want)
		}
	}
	// One request line each, naming the registered pattern, never the
	// profile asked for or its query.
	if logged := log.String(); !strings.Contains(logged, `"route":"/api/v1/debug/pprof/"`) || strings.Contains(logged, "debug=2") ||
		strings.Contains(logged, "goroutine") || strings.Contains(logged, testSecret) {
		t.Fatalf("the request lines: %s", logged)
	}
}

// A server built without an admission view (no agent listener) answers a
// disabled gate's snapshot.
func TestTheAdmissionViewWithoutAGateIsDisabled(t *testing.T) {
	s, _, _ := apiServer(t)
	rec := debugGet(s, http.MethodGet, diagnostics.AdmissionPath, "Bearer "+testSecret)
	var got map[string]any
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got["enabled"] != false || got["in_flight"] != float64(0) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}
