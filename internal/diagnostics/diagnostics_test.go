package diagnostics

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/telflo/opamp-server/middleware"
)

// A profile's or a trace's `seconds`, when given, must be a whole number
// from 1 to 25, under the 30 s write timeout (pprof's own default, 30,
// would reach it), else 400 and nothing profiled; a CPU profile without one
// takes 25, a trace without one pprof's default.
func TestAProfilesSecondsAreAWholeNumberFrom1To25(t *testing.T) {
	for _, c := range []struct {
		always      bool
		asked, sent string
		status      int
	}{
		{true, "", "25", http.StatusOK}, {true, "1", "1", http.StatusOK}, {true, "10", "10", http.StatusOK},
		{true, "25", "25", http.StatusOK}, {false, "", "", http.StatusOK}, {false, "7", "7", http.StatusOK},
		{true, "26", "", http.StatusBadRequest}, {true, "3600", "", http.StatusBadRequest},
		{true, "0", "", http.StatusBadRequest}, {true, "-1", "", http.StatusBadRequest},
		{true, "010", "", http.StatusBadRequest}, {true, "+5", "", http.StatusBadRequest},
		{true, "0.5", "", http.StatusBadRequest}, {true, "NaN", "", http.StatusBadRequest},
		{true, "1e1", "", http.StatusBadRequest}, {true, "not-a-number", "", http.StatusBadRequest},
		{true, "seconds=", "", http.StatusBadRequest}, {true, "5&seconds=6", "", http.StatusBadRequest},
		{false, "0.5", "", http.StatusBadRequest}, {false, "40", "", http.StatusBadRequest},
	} {
		seen, reached := "", false
		record := func(w http.ResponseWriter, r *http.Request) { seen, reached = r.URL.Query().Get("seconds"), true }
		target := PprofPrefix + "profile"
		switch {
		case c.asked == "seconds=":
			target += "?seconds="
		case c.asked != "":
			target += "?seconds=" + c.asked
		}
		rec := httptest.NewRecorder()
		capSeconds(record, c.always)(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != c.status || reached != (c.status == http.StatusOK) || seen != c.sent {
			t.Errorf("always %v, asked %q: %d, reached %v, sent %q; want %d, %q", c.always, c.asked, rec.Code, reached, seen,
				c.status, c.sent)
		}
		if c.status == http.StatusBadRequest && (!strings.Contains(rec.Body.String(), "invalid_request") ||
			strings.Contains(rec.Body.String(), "not-a-number")) {
			t.Errorf("asked %q: the refusal %q", c.asked, rec.Body.String())
		}
	}
}

// Handler, a listener of its own, serves the index, a goroutine dump and
// the admission view, refuses cmdline, and answers 404 not_found to
// another method or path.
func TestTheHandlerServesBothRoutesAndNothingElse(t *testing.T) {
	snapshot := middleware.AdmissionSnapshot{Enabled: true, MaxInFlight: 100, InFlight: 7,
		OldestInFlightAge: 4*time.Minute + 3500*time.Millisecond, StaleThreshold: 10 * time.Second, ShedTotal: 9}
	h := Handler(func() middleware.AdmissionSnapshot { return snapshot })
	get := func(method, target string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
		return rec
	}
	if rec := get(http.MethodGet, PprofPrefix); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "goroutine") {
		t.Fatalf("the index: %d", rec.Code)
	}
	if rec := get(http.MethodGet, PprofPrefix+"goroutine?debug=2"); rec.Code != http.StatusOK || !strings.HasPrefix(rec.Body.String(), "goroutine ") {
		t.Fatalf("a goroutine dump: %d", rec.Code)
	}
	rec := get(http.MethodGet, AdmissionPath)
	var got map[string]any
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" || json.Unmarshal(rec.Body.Bytes(), &got) != nil ||
		got["in_flight"] != float64(7) || got["oldest_in_flight_age"] != "4m3.5s" || got["stale_threshold"] != "10s" {
		t.Fatalf("the admission view: %d %s", rec.Code, rec.Body.String())
	}
	for _, c := range []struct{ method, target string }{
		{http.MethodGet, PprofPrefix + "cmdline"}, {http.MethodPost, AdmissionPath}, {http.MethodGet, "/"}, {http.MethodGet, "/api/v1/organizations"},
	} {
		if rec := get(c.method, c.target); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"not_found"`) {
			t.Errorf("%s %s: %d %s", c.method, c.target, rec.Code, rec.Body.String())
		}
	}
}

// A nil snapshot, a process without an agent endpoint, answers a disabled
// gate's.
func TestTheAdmissionViewWithoutAGateIsDisabled(t *testing.T) {
	rec := httptest.NewRecorder()
	Admission(nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, AdmissionPath, nil))
	var got map[string]any
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got["enabled"] != false {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}
