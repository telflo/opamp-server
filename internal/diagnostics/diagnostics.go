// Package diagnostics serves an operator's diagnostics: Go's runtime
// profiles and the agent endpoint's admission view. It authenticates
// nothing: the management API mounts these handlers behind its secret, and
// `serve agent` serves them on a loopback-only listener of their own. Both
// read only process state, never the database. They are an operator's, not
// the platform's, so api/openapi.yaml leaves them out.
package diagnostics

import (
	"encoding/json"
	"net/http"
	"net/http/pprof"
	"strconv"
	"strings"
	"time"

	"github.com/telflo/telflo-opamp-server/internal/apierror"
	"github.com/telflo/telflo-opamp-server/middleware"
)

const (
	// Prefix is where the diagnostic routes live, wherever they are mounted.
	Prefix = "/api/v1/debug/"
	// PprofPrefix is where the profiles are served: the index, each named
	// profile (goroutine, heap, allocs, block, mutex, threadcreate),
	// profile (CPU), symbol and trace. cmdline is not served: argv has
	// nothing diagnostic and is one more thing to leak.
	PprofPrefix = Prefix + "pprof/"
	// AdmissionPath is the admission view: the agent endpoint's concurrency
	// gate (middleware.AdmissionSnapshot) as JSON.
	AdmissionPath = Prefix + "admission"

	// MaxProfileSeconds is the longest CPU profile or trace served, under a
	// listener's 30 s write timeout (pprof's own default, 30, would reach
	// it): `seconds` must be a whole number from 1 to it, and a CPU profile
	// asked without one takes it.
	MaxProfileSeconds = 25
)

// Patterns are the routes' ServeMux patterns, for a router that registers
// them one by one (the management API's, which authorizes each).
const (
	ProfilesPattern  = http.MethodGet + " " + PprofPrefix
	AdmissionPattern = http.MethodGet + " " + AdmissionPath
)

// Handler serves both routes and answers 404 not_found to anything else:
// a listener of their own.
func Handler(admission func() middleware.AdmissionSnapshot) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(ProfilesPattern, Profiles())
	mux.Handle(AdmissionPattern, Admission(admission))
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { apierror.WriteNotFound(w) })
	return mux
}

// Profiles serves Go's runtime profiles under PprofPrefix, so a process
// whose agent port sheds every poll can still be read from outside: a
// goroutine dump, the heap.
//
// pprof.Index dispatches to a named profile only when the path carries its
// own "/debug/pprof/" prefix, so the mux is registered at the canonical
// paths and "/api/v1" is stripped before dispatch.
func Profiles() http.Handler {
	const canonical = "/debug/pprof/"
	mux := http.NewServeMux()
	mux.HandleFunc(canonical, pprof.Index)
	mux.HandleFunc(canonical+"profile", capSeconds(pprof.Profile, true))
	mux.HandleFunc(canonical+"symbol", pprof.Symbol)
	mux.HandleFunc(canonical+"trace", capSeconds(pprof.Trace, false))
	rebased := http.StripPrefix(strings.TrimSuffix(PprofPrefix, canonical), mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/cmdline") {
			apierror.WriteNotFound(w)
			return
		}
		rebased.ServeHTTP(w, r)
	})
}

// capSeconds bounds the request's `seconds`: when given, it must be a whole
// number from 1 to MaxProfileSeconds, written in decimal digits alone (no
// sign, fraction, exponent, space or leading zero), else the request
// answers 400 invalid_request and nothing is profiled (pprof itself read
// "NaN", "0.5" or "1e3" as it could). Without one, a CPU profile (always)
// takes MaxProfileSeconds, pprof's own default of 30 being too long; a
// trace takes pprof's default of one second.
func capSeconds(next http.HandlerFunc, always bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if !query.Has("seconds") {
			if always {
				query.Set("seconds", strconv.Itoa(MaxProfileSeconds))
				capped := r.Clone(r.Context())
				capped.URL.RawQuery = query.Encode()
				r = capped
			}
			next(w, r)
			return
		}
		if !profileSeconds(query["seconds"]) {
			apierror.Write(w, http.StatusBadRequest, apierror.CodeInvalidRequest,
				"seconds must be a whole number from 1 to "+strconv.Itoa(MaxProfileSeconds)+".")
			return
		}
		next(w, r)
	}
}

// profileSeconds reports whether values is one whole number from 1 to
// MaxProfileSeconds in decimal digits alone.
func profileSeconds(values []string) bool {
	if len(values) != 1 || values[0] == "" || values[0][0] == '0' {
		return false
	}
	for _, c := range values[0] {
		if c < '0' || c > '9' {
			return false
		}
	}
	n, err := strconv.Atoi(values[0])
	return err == nil && n >= 1 && n <= MaxProfileSeconds
}

// Admission renders the admission gate's live state as JSON: readable
// before saturation (watch in_flight climb) and after a load generator has
// been stopped (did the slots drain, or are they still held: a throughput
// ceiling against a wedge). Durations are Go duration strings ("4m3.5s").
// A nil snapshot, a process without an agent endpoint, answers a disabled
// gate's.
func Admission(snapshot func() middleware.AdmissionSnapshot) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		var s middleware.AdmissionSnapshot
		if snapshot != nil {
			s = snapshot()
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(admissionReply{
			AdmissionSnapshot: s,
			OldestInFlightAge: s.OldestInFlightAge.Round(time.Millisecond).String(),
			StaleThreshold:    s.StaleThreshold.String(),
		})
	})
}

// admissionReply overlays string renderings of the durations the snapshot
// leaves out of its JSON.
type admissionReply struct {
	middleware.AdmissionSnapshot
	OldestInFlightAge string `json:"oldest_in_flight_age"`
	StaleThreshold    string `json:"stale_threshold"`
}
