// Package agent is the agent endpoint as a component: the port collectors
// poll, apart from the management API's, behind its own load balancer. It
// reads its own settings (settings.go, limits.go), builds the middleware
// stack around the poll handler (middleware.AgentStack around
// handlers.AgentHandler), and routes the listener's paths (routes.go).
//
// It opens no listener and runs no process: package serve does, with this
// endpoint alone (`serve agent`) or beside the management API (`serve`).
package agent

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/telflo/telflo-opamp-server/handlers"
	"github.com/telflo/telflo-opamp-server/middleware"
	"github.com/telflo/telflo-opamp-server/store/collectors"
	"github.com/telflo/telflo-opamp-server/store/enrollmenttokens"
	"github.com/telflo/telflo-opamp-server/store/organizations"
	"github.com/telflo/telflo-opamp-server/store/variables"
	"github.com/telflo/telflo-opamp-server/tokens"
)

// Stores are the stores the agent endpoint reads and writes, all on one
// database.
type Stores struct {
	// Organizations gives each organization's request rate.
	Organizations organizations.Store
	// Collectors are the collectors a poll enrolls and updates.
	Collectors collectors.Store
	// Tokens are the enrollment tokens a poll authenticates with.
	Tokens enrollmenttokens.Store
	// Variables fill in a configuration's references.
	Variables variables.Store
}

// Options are what an Endpoint is built from. serve passes the real ones;
// tests pass their own.
type Options struct {
	Settings Settings
	Stores   Stores
	// Keys read the bearers collectors present and encrypt the rotate
	// tokens pushed to them. Required.
	Keys *tokens.Keyring
	// Ready answers /ready: the process's readiness, which package serve
	// owns. Required.
	Ready http.Handler
	// Now is the only source of time. Required.
	Now func() time.Time
	// Logger receives the endpoint's log lines. Required.
	Logger *slog.Logger
}

// Endpoint is the agent listener's handler and the concurrency gate in
// front of it.
type Endpoint struct {
	handler     http.Handler
	concurrency *middleware.ConcurrencyLimiter
}

// New builds the agent endpoint. Its parts are built once and shared: the
// Authenticator authenticates in the stack and answers a credential lost
// mid-poll in the handler; the collector guard is the stack's for a request
// carrying its instance uid in a header, else the handler's; the
// concurrency gate is the stack's and the admission view's (Admission).
func New(options Options) (*Endpoint, error) {
	switch {
	case options.Stores.Organizations == nil, options.Stores.Collectors == nil,
		options.Stores.Tokens == nil, options.Stores.Variables == nil:
		return nil, errors.New("agent: a store is missing")
	case options.Keys == nil:
		return nil, errors.New("agent: no encryption keys")
	case options.Ready == nil:
		return nil, errors.New("agent: no readiness")
	case options.Now == nil:
		return nil, errors.New("agent: no clock")
	case options.Logger == nil:
		return nil, errors.New("agent: no logger")
	}
	settings, now, logger := options.Settings, options.Now, options.Logger
	limits := settings.Limits
	authenticator := middleware.NewAuthenticator(options.Keys, options.Stores.Tokens, now, logger)
	concurrency := middleware.NewConcurrencyLimiter(limits.MaxConcurrentRequests, now, logger)
	guard := middleware.NewCollectorGuard(limits.CollectorRequestsPerMinute, now, logger)
	poll, err := handlers.NewAgentHandler(handlers.Options{
		Collectors:    options.Stores.Collectors,
		Tokens:        options.Stores.Tokens,
		Variables:     options.Stores.Variables,
		Keyring:       options.Keys,
		Authenticator: authenticator,
		Guard:         guard,
		ServerURL:     settings.ServerURL,
		// The handler's own check refuses an interval under a second: the
		// settings allow 10 to 300.
		HeartbeatInterval: settings.HeartbeatInterval,
		Cache:             handlers.NewYamlCache(),
		// The pod's own, keyed by the organization's counter, so it never
		// serves values a change has replaced.
		VariablesCache: handlers.NewVariablesCache(),
		Now:            now,
		Logger:         logger,
	})
	if err != nil {
		return nil, err
	}
	stack := middleware.AgentStack{
		Concurrency:   concurrency,
		Authenticator: authenticator,
		OrganizationConcurrency: middleware.NewOrganizationConcurrency(limits.MaxConcurrentRequestsPerOrganization,
			now, logger),
		Guard:         guard,
		Organizations: middleware.NewOrganizationLimiter(options.Stores.Organizations, limits.DefaultRequestsPerMinute, now, logger),
		Now:           now,
		Logger:        logger,
	}
	return &Endpoint{
		handler:     routes(stack.Handler(poll), options.Ready),
		concurrency: concurrency,
	}, nil
}

// Handler is the agent listener's handler (routes.go).
func (e *Endpoint) Handler() http.Handler {
	return e.handler
}

// Admission is the concurrency gate's live state, for the admission view.
func (e *Endpoint) Admission() middleware.AdmissionSnapshot {
	return e.concurrency.Snapshot()
}
