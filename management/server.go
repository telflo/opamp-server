// Package management is the opamp-server's management API, on its own port
// behind a shared secret, or an API key limited to its organizations
// (api_keys.go): the platform calls it; nothing on the internet should
// reach it. It is a component: package serve opens its listener, alone
// (`serve management`) or beside the agent endpoint's (`serve`).
//
// A request goes through, outermost first:
//
//	GET /health          200 "ok": no secret, no database, no log line
//	GET /ready           the process's readiness (Options.Ready): no secret,
//	                     no log line
//	observe              the request's log line and server span: method,
//	                     route pattern, status, duration; nothing else
//	recoverPanics        a panic answers 500 internal_error, no detail
//	/api/ ...
//	  authenticate       Authorization: Bearer <secret or API key>, or 401
//	  limitBody          bodies capped at 2 MiB, or 413
//	  authorize          what an API key may call on the route, or 403
//	                     (404 for another organization)
//	  the API's routes   anything unmatched (path or method): 404 not_found
//	anything else        404 not_found
//
// Every error answers the platform's envelope,
// {"error":{"code":"…","message":"…"}}.
//
// The routes: organizations (organizations.go), publications
// (publications.go), and the fleet model's: fleets, collector groups and
// enrollment tokens (fleets.go, collector_groups.go, enrollment_tokens.go),
// collectors and their errors (collectors.go), an organization's limits
// (limits.go), and what uses a configuration's published versions
// (usage.go); an organization's variables, variable groups and their
// overrides (variables.go); the API keys (api_keys.go); and an operator's
// diagnostics, the profiles and
// the agent listener's admission view, left out of api/openapi.yaml
// (debug.go, package diagnostics).
package management

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/telflo/opamp-server/middleware"
	"github.com/telflo/opamp-server/store/apikeys"
	"github.com/telflo/opamp-server/store/collectorgroups"
	"github.com/telflo/opamp-server/store/collectors"
	"github.com/telflo/opamp-server/store/enrollmenttokens"
	"github.com/telflo/opamp-server/store/fleets"
	"github.com/telflo/opamp-server/store/organizations"
	"github.com/telflo/opamp-server/store/publications"
	"github.com/telflo/opamp-server/store/variables"
	"github.com/telflo/opamp-server/tokens"
)

// apiPrefix is the subtree behind the secret (or an API key).
const apiPrefix = "/api/"

// Options are what a Server is built from. serve passes the real ones;
// tests pass their own.
type Options struct {
	// Secrets are the accepted shared secrets. Required.
	Secrets *Secrets
	// Organizations is the organization registry, on the same database as
	// Publications. Required.
	Organizations organizations.Store
	// Publications is the publications store, on the database once its
	// startup checks pass. /health never touches it. Required.
	Publications publications.Store
	// Fleets, CollectorGroups and EnrollmentTokens are the fleet model's
	// stores, on the same database as Publications. Required.
	Fleets           fleets.Store
	CollectorGroups  collectorgroups.Store
	EnrollmentTokens enrollmenttokens.Store
	// Collectors is the agent side's store, on the same database: the
	// management API reads, changes and deletes collectors through it.
	// Required.
	Collectors collectors.Store
	// Variables is the organization variables' store, on the same database:
	// its variables, variable groups and overrides. Required.
	Variables variables.Store
	// APIKeys is the management API's keys' store, on the same database:
	// authentication reads it beside the secrets. Required.
	APIKeys apikeys.Store
	// Keys mint the enrollment tokens' bearers. Required.
	Keys *tokens.Keyring
	// Clock is the only source of time in this package (CONTRIBUTING.md,
	// "Time"). Required.
	Clock func() time.Time
	// Logger receives one JSON line per request and nothing it shouldn't.
	// Required.
	Logger *slog.Logger
	// Admission is the agent listener's concurrency gate's state, for the
	// admission view (debug.go); nil, a process without an agent endpoint,
	// answers a disabled gate's.
	Admission func() middleware.AdmissionSnapshot
	// Ready answers /ready: the process's readiness, which package serve
	// owns (200 once serving while the database answers, 503 otherwise).
	// nil answers 200: a server built without a process, as tests build it.
	Ready http.Handler
	// DeleteTimeout bounds an organization's, a fleet's or a group's delete
	// once started (deletes.go); zero is DeleteTimeout, 5 minutes. Tests
	// pass their own.
	DeleteTimeout time.Duration
}

// Server is the management API's handler and what it is built on.
type Server struct {
	secrets       *Secrets
	organizations organizations.Store
	publications  publications.Store
	fleets        fleets.Store
	groups        collectorgroups.Store
	// enrollmentTokens is the tokens' store; keys mint their bearers.
	enrollmentTokens enrollmenttokens.Store
	collectors       collectors.Store
	variables        variables.Store
	apiKeys          apikeys.Store
	keys             *tokens.Keyring
	clock            func() time.Time
	logger           *slog.Logger
	admission        func() middleware.AdmissionSnapshot
	// deleteTimeout bounds an organization's or a fleet's delete
	// (deletes.go).
	deleteTimeout time.Duration
	// deleting is the organizations and fleets this replica is deleting
	// (deletes.go).
	deleting deletesInProgress

	// outer routes /health and everything else; inner routes everything but
	// /health; api routes the subtree behind the secret. Each remembers its
	// patterns, so a log line or a span names a registered pattern and never
	// a path a client wrote, and the conformance test can list the routes.
	outer   *router
	inner   *router
	api     *router
	handler http.Handler
}

// New builds the management API's handler.
func New(options Options) (*Server, error) {
	switch {
	case options.Secrets == nil:
		return nil, errors.New("management: no secrets")
	case options.Organizations == nil:
		return nil, errors.New("management: no organizations store")
	case options.Publications == nil:
		return nil, errors.New("management: no publications store")
	case options.Fleets == nil:
		return nil, errors.New("management: no fleets store")
	case options.CollectorGroups == nil:
		return nil, errors.New("management: no collector groups store")
	case options.EnrollmentTokens == nil:
		return nil, errors.New("management: no enrollment tokens store")
	case options.Collectors == nil:
		return nil, errors.New("management: no collectors store")
	case options.Variables == nil:
		return nil, errors.New("management: no variables store")
	case options.APIKeys == nil:
		return nil, errors.New("management: no API keys store")
	case options.Keys == nil:
		return nil, errors.New("management: no encryption keys")
	case options.Clock == nil:
		return nil, errors.New("management: no clock")
	case options.Logger == nil:
		return nil, errors.New("management: no logger")
	case options.DeleteTimeout < 0:
		return nil, errors.New("management: a negative delete timeout")
	}
	deleteTimeout := options.DeleteTimeout
	if deleteTimeout == 0 {
		deleteTimeout = DeleteTimeout
	}
	s := &Server{
		secrets:          options.Secrets,
		organizations:    options.Organizations,
		publications:     options.Publications,
		fleets:           options.Fleets,
		groups:           options.CollectorGroups,
		enrollmentTokens: options.EnrollmentTokens,
		collectors:       options.Collectors,
		variables:        options.Variables,
		apiKeys:          options.APIKeys,
		keys:             options.Keys,
		clock:            options.Clock,
		logger:           options.Logger,
		admission:        options.Admission,
		deleteTimeout:    deleteTimeout,
		inner:            newRouter(),
		api:              newRouter(),
	}
	// Each route of the API answers an API key only what it may call.
	s.api.wrap = s.authorize

	// After authentication, a request no route matches, by path or by
	// method, answers 404 not_found.
	s.api.handle(apiPrefix, http.HandlerFunc(notFound))
	s.registerOrganizations()
	s.registerPublications()
	s.registerUsage()
	s.registerLimits()
	s.registerFleets()
	s.registerVariables()
	s.registerAPIKeys()
	s.registerDebug()

	s.inner.handle(apiPrefix, s.authenticate(limitBody(s.api.mux)))
	s.inner.handle("/", http.HandlerFunc(notFound))

	s.outer = newRouter()
	// Liveness only, outside the logging: health checks run every few
	// seconds and would drown the log.
	s.outer.handle("GET /health", http.HandlerFunc(health))
	// Readiness likewise: the kubelet asks it every few seconds.
	ready := options.Ready
	if ready == nil {
		ready = http.HandlerFunc(alwaysReady)
	}
	s.outer.handle("GET /ready", ready)
	s.outer.handle("/", s.observe(s.recoverPanics(s.inner.mux)))
	s.handler = s.outer.mux
	return s, nil
}

// routes lists every pattern registered on the server's three routers, the
// catch-alls included: what the conformance test holds api/openapi.yaml
// against.
func (s *Server) routes() []string {
	var patterns []string
	for _, r := range []*router{s.outer, s.inner, s.api} {
		patterns = append(patterns, r.patterns...)
	}
	return patterns
}

// Handler is the management listener's handler.
func (s *Server) Handler() http.Handler {
	return s.handler
}

// health answers liveness: 200 "ok", with no secret and no database.
func health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok"))
}

// alwaysReady is /ready for a server built without a process: 200
// "ready".
func alwaysReady(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte("ready\n"))
}

func notFound(w http.ResponseWriter, _ *http.Request) {
	writeNotFound(w)
}

// routePattern is the registered pattern a request matches, without its
// method: what a log line and a span call the route. A request that matches
// none (a path ServeMux redirects to its clean form) is "unmatched".
func (s *Server) routePattern(r *http.Request) string {
	pattern := s.inner.match(r)
	if pattern == apiPrefix {
		pattern = s.api.match(r)
	}
	if pattern == "" {
		return "unmatched"
	}
	if _, path, found := strings.Cut(pattern, " "); found {
		return path
	}
	return pattern
}

// router is a ServeMux that remembers what was registered on it, and,
// with wrap set, wraps each handler with what its pattern calls for.
type router struct {
	mux        *http.ServeMux
	patterns   []string
	registered map[string]bool
	wrap       func(pattern string, handler http.Handler) http.Handler
}

func newRouter() *router {
	return &router{mux: http.NewServeMux(), registered: map[string]bool{}}
}

func (r *router) handle(pattern string, handler http.Handler) {
	if r.wrap != nil {
		handler = r.wrap(pattern, handler)
	}
	r.mux.Handle(pattern, handler)
	r.patterns = append(r.patterns, pattern)
	r.registered[pattern] = true
}

// match is the registered pattern ServeMux would route request to, or ""
// when it would answer something else (a redirect to the clean path, whose
// "pattern" is that path).
func (r *router) match(request *http.Request) string {
	_, pattern := r.mux.Handler(request)
	if r.registered[pattern] {
		return pattern
	}
	return ""
}
