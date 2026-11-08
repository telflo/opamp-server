package serve

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/telflo/opamp-server/agent"
	"github.com/telflo/opamp-server/internal/diagnostics"
	"github.com/telflo/opamp-server/management"
	"github.com/telflo/opamp-server/middleware"
	"github.com/telflo/opamp-server/store/apikeys"
	"github.com/telflo/opamp-server/store/collectorgroups"
	"github.com/telflo/opamp-server/store/collectors"
	"github.com/telflo/opamp-server/store/enrollmenttokens"
	"github.com/telflo/opamp-server/store/fleets"
	"github.com/telflo/opamp-server/store/organizations"
	"github.com/telflo/opamp-server/store/publications"
	"github.com/telflo/opamp-server/store/variables"
)

// replicaStores are the stores a replica is built on: serve's are all on
// the database (postgresStores); a test may wrap one.
type replicaStores struct {
	organizations    organizations.Store
	publications     publications.Store
	fleets           fleets.Store
	collectorGroups  collectorgroups.Store
	enrollmentTokens enrollmenttokens.Store
	collectors       collectors.Store
	variables        variables.Store
	apiKeys          apikeys.Store
}

// postgresStores are every store on pool.
func postgresStores(pool *pgxpool.Pool) replicaStores {
	return replicaStores{
		organizations:    organizations.NewPostgres(pool),
		publications:     publications.NewPostgres(pool),
		fleets:           fleets.NewPostgres(pool),
		collectorGroups:  collectorgroups.NewPostgres(pool),
		enrollmentTokens: enrollmenttokens.NewPostgres(pool),
		collectors:       collectors.NewPostgres(pool),
		variables:        variables.NewPostgres(pool),
		apiKeys:          apikeys.NewPostgres(pool),
	}
}

// replica is everything one serve process runs (one pod, of however many):
// the components its role runs, their readiness, and, with the agent
// endpoint, the error history's cleanup and the diagnostics listener when
// set. It keeps nothing the database doesn't hold but its caches and its
// limiters' counts, each its own.
type replica struct {
	// management is the management API, nil when the role runs none.
	management *management.Server
	// agent is the agent listener's handler, nil when the role runs none.
	agent http.Handler
	// diagnostics is the agent's diagnostics listener's handler, nil
	// unless the agent endpoint runs with OPAMP_AGENT_DIAGNOSTICS_PORT set.
	diagnostics http.Handler
	readiness   *readiness
	// cleanup is the error history's cleanup, nil without the agent
	// endpoint: collectors' errors arrive only through it.
	cleanup *cleanupJob
	// drainTimer is the shutdown drain's timer: nil for time.After; a test
	// sets its own to end the drain when it chooses.
	drainTimer func(time.Duration) <-chan time.Time
}

// newReplica builds a replica from serve's settings, on stores, with
// readiness asking ping, on the clock now: the components settings.Role
// runs, the agent endpoint (package agent) and the management API (package
// management), whose admission view reads the agent endpoint's
// concurrency gate when both run in this process. Both answer /ready from
// the one readiness.
func newReplica(settings Settings, stores replicaStores, ping func(context.Context) error,
	now func() time.Time, logger *slog.Logger) (*replica, error) {
	if ping == nil {
		return nil, errors.New("serve: no readiness ping")
	}
	r := &replica{readiness: newReadiness(ping, now)}
	var admission func() middleware.AdmissionSnapshot
	if settings.Role.runsAgent() {
		endpoint, err := agent.New(agent.Options{
			Settings: settings.Agent,
			Stores: agent.Stores{
				Organizations: stores.organizations,
				Collectors:    stores.collectors,
				Tokens:        stores.enrollmentTokens,
				Variables:     stores.variables,
			},
			Keys:   settings.Keys,
			Ready:  r.readiness,
			Now:    now,
			Logger: logger,
		})
		if err != nil {
			return nil, err
		}
		r.agent = endpoint.Handler()
		admission = endpoint.Admission
		r.cleanup = &cleanupJob{store: stores.collectors, now: now, logger: logger}
		if settings.Agent.Diagnostics != nil {
			r.diagnostics = diagnostics.Handler(admission)
		}
	}
	if settings.Role.runsManagement() {
		server, err := management.New(management.Options{
			Secrets:          settings.Management.Secrets,
			Organizations:    stores.organizations,
			Publications:     stores.publications,
			Fleets:           stores.fleets,
			CollectorGroups:  stores.collectorGroups,
			EnrollmentTokens: stores.enrollmentTokens,
			Collectors:       stores.collectors,
			Variables:        stores.variables,
			APIKeys:          stores.apiKeys,
			Keys:             settings.Keys,
			Clock:            now,
			Logger:           logger,
			Admission:        admission,
			Ready:            r.readiness,
		})
		if err != nil {
			return nil, err
		}
		r.management = server
	}
	return r, nil
}
