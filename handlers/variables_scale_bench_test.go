package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"

	"github.com/telflo/opamp-server/middleware"
	"github.com/telflo/opamp-server/store/collectorgroups"
	"github.com/telflo/opamp-server/store/collectors"
	"github.com/telflo/opamp-server/store/database/databasetest"
	"github.com/telflo/opamp-server/store/enrollmenttokens"
	"github.com/telflo/opamp-server/store/fleets"
	"github.com/telflo/opamp-server/store/organizations"
	"github.com/telflo/opamp-server/store/orgvariables"
	"github.com/telflo/opamp-server/store/publications"
	"github.com/telflo/opamp-server/store/storetest"
	"github.com/telflo/opamp-server/store/variables"
	"github.com/telflo/opamp-server/tokens"
)

// The 10,000-collector measured check. It records numbers and has no time
// threshold: the counts of an unused value's change are its only
// assertions. It runs only when ScaleCheckDirVariable names a folder (its
// results and EXPLAIN plans are written there) and only as
//
//	OPAMP_SCALE_CHECK_DIR=<folder> go test -run '^$' -bench TenThousand -benchtime 1x ./handlers/
//
// on a Postgres of one's own (OPAMP_TEST_ADMIN_DATABASE_URL), without -race:
// every sub-benchmark changes the fixture, so each runs exactly once.
//
// The fixture: 1 organization and 1 fleet; 20 groups, 10 of them bound to 5
// variable groups; 10,000 collectors (admin SQL, generate_series), 100 of
// them pinned to a 6th variable group; 150 variables (the most an
// organization may have); one published version of scaleYAML referencing 10
// names, assigned to every group; every collector served and marked (In
// sync) by the server's own polls, then VACUUM ANALYZE.

// ScaleCheckDirVariable names the folder the check writes to; unset, the
// check is skipped.
const ScaleCheckDirVariable = "OPAMP_SCALE_CHECK_DIR"

// scaleDatabase is the check's own database, apart from the package's tests'.
const scaleDatabase = "telflo_opamp_handlers_scale_test"

const (
	scaleCollectors   = 10_000
	scaleGroups       = 20
	scaleBoundGroups  = 10
	scaleVariables    = 150
	scalePinned       = 100
	scaleRepeats      = 50
	scaleReplicas     = 2
	scaleWorkers      = 16 // in all, shared out between the replicas
	scalePollsPerSec  = 330
	scaleRun          = 60 * time.Second
	scaleChangeAt     = 20 * time.Second
	scaleWindow       = 5 * time.Second
	scaleFollowUp     = time.Second // a pushed collector's status, as a supervisor reports it
	scalePoolMaxConns = 10          // README's example pool_max_conns, per replica
	scaleFillInRounds = 20_000
	// scaleLagLimit is the most a poll may start after it was due before
	// the run is called saturated: the load generator fell behind, so its
	// latencies are the machine's, not the server's.
	scaleLagLimit = 100 * time.Millisecond
)

// The names: V001 to V150. V001 to V010 are the ones scaleYAML uses; each
// variable group overrides V001 to V005 and V100 to V119, with a reset row
// on V006. usedName's change reaches the collectors whose pin or group
// doesn't override it (about half); herdName, which nothing overrides,
// reaches every collector (the push herd); unusedName reaches none.
const (
	usedName   = "V001"
	herdName   = "V010"
	unusedName = "V140"
)

func scaleName(i int) string { return fmt.Sprintf("V%03d", i) }

// scaleValues are realistic values of the names scaleYAML uses (one with a
// `$`, written as `$$`).
var scaleValues = map[string]string{
	"V001": "otel-gateway.us-east-1.internal:4317",
	"V002": "us-east-1",
	"V003": "production",
	"V004": "payments",
	"V005": "edge-cluster-7",
	"V006": "0.25",
	"V007": "512",
	"V008": "tenant-4821",
	"V009": "x-telflo-tenant",
	"V010": "https://ingest.example.com/v1/traces?k=$team",
}

// scaleYAML is a gateway's configuration of a realistic size (about 4 KB),
// referencing V001 to V010, some more than once.
const scaleYAML = `extensions:
  health_check:
    endpoint: 0.0.0.0:13133
  pprof:
    endpoint: localhost:1777
  zpages:
    endpoint: localhost:55679
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
        max_recv_msg_size_mib: 16
      http:
        endpoint: 0.0.0.0:4318
        cors:
          allowed_origins: ["https://*.example.com"]
  hostmetrics:
    collection_interval: 30s
    scrapers:
      cpu: {}
      disk: {}
      filesystem: {}
      load: {}
      memory: {}
      network: {}
      paging: {}
  filelog:
    include: [/var/log/payments/*.log]
    start_at: end
    operators:
      - type: json_parser
        timestamp:
          parse_from: attributes.time
          layout: '%Y-%m-%dT%H:%M:%S.%LZ'
      - type: severity_parser
        parse_from: attributes.level
processors:
  memory_limiter:
    check_interval: 1s
    limit_mib: ${org:V007}
    spike_limit_mib: 128
  batch:
    send_batch_size: 8192
    send_batch_max_size: 16384
    timeout: 5s
  resource:
    attributes:
      - key: deployment.environment
        value: ${org:V003}
        action: upsert
      - key: cloud.region
        value: ${org:V002}
        action: upsert
      - key: service.namespace
        value: ${org:V004}
        action: upsert
      - key: k8s.cluster.name
        value: ${org:V005}
        action: upsert
  attributes/tenant:
    actions:
      - key: tenant.id
        value: ${org:V008}
        action: insert
      - key: http.request.header.authorization
        action: delete
      - key: db.statement
        action: hash
  probabilistic_sampler:
    sampling_percentage: 25
    hash_seed: 22
  tail_sampling:
    decision_wait: 10s
    num_traces: 50000
    policies:
      - name: errors
        type: status_code
        status_code: {status_codes: [ERROR]}
      - name: slow
        type: latency
        latency: {threshold_ms: 750}
      - name: rest
        type: probabilistic
        probabilistic: {sampling_percentage: 10}
  filter/health:
    error_mode: ignore
    traces:
      span:
        - 'attributes["http.route"] == "/healthz"'
        - 'attributes["http.route"] == "/readyz"'
  transform/redact:
    error_mode: ignore
    log_statements:
      - context: log
        statements:
          - replace_pattern(body, "card=[0-9]{12,19}", "card=****")
exporters:
  otlp/gateway:
    endpoint: ${org:V001}
    tls:
      insecure: false
      ca_file: /etc/otelcol/ca.pem
    headers:
      ${org:V009}: ${org:V008}
    sending_queue:
      enabled: true
      num_consumers: 10
      queue_size: 5000
    retry_on_failure:
      enabled: true
      initial_interval: 5s
      max_interval: 30s
      max_elapsed_time: 300s
  otlphttp/traces:
    traces_endpoint: ${org:V010}
    compression: gzip
    headers:
      ${org:V009}: ${org:V008}
  debug:
    verbosity: basic
    sampling_initial: 5
    sampling_thereafter: ${org:V006}
service:
  extensions: [health_check, pprof, zpages]
  telemetry:
    logs:
      level: info
    metrics:
      level: normal
  pipelines:
    traces:
      receivers: [otlp]
      processors: [memory_limiter, filter/health, resource, attributes/tenant, tail_sampling, batch]
      exporters: [otlp/gateway, otlphttp/traces]
    metrics:
      receivers: [otlp, hostmetrics]
      processors: [memory_limiter, resource, batch]
      exporters: [otlp/gateway]
    logs:
      receivers: [otlp, filelog]
      processors: [memory_limiter, transform/redact, resource, attributes/tenant, batch]
      exporters: [otlp/gateway, debug]
`

// BenchmarkTenThousandCollectors is the measured check (see the file's
// comment). Its sub-benchmarks run in order on one fixture.
func BenchmarkTenThousandCollectors(b *testing.B) {
	dir := os.Getenv(ScaleCheckDirVariable)
	if dir == "" {
		b.Skipf("%s is not set: the 10,000-collector check runs only when asked", ScaleCheckDirVariable)
	}
	if f := flag.Lookup("test.benchtime"); f == nil || f.Value.String() != "1x" {
		b.Skip("run with -benchtime 1x: each sub-benchmark changes the fixture and runs once")
	}
	f := newScaleFixture(b, dir)
	b.Run("fleet counts", f.fleetCounts)
	b.Run("save", f.save)
	b.Run("unused value changed", f.unusedValueChanged)
	b.Run("poll during a change", f.pollDuringAChange)
	b.Run("fill-in CPU", f.fillInCPU)
	f.writeResults(b)
}

// scaleCollector is one simulated collector: its polls are serialized, as
// one supervisor's are.
type scaleCollector struct {
	mu      sync.Mutex
	id, uid uuid.UUID
	replica int
	seq     uint64
	// ack is the hash of the configuration last pushed, which its next
	// message reports APPLIED with its bytes (ackBody) as the effective
	// configuration; nil once reported.
	ack, ackBody []byte
}

// scaleReplica is one pod: its own pool, stores, caches and guard.
type scaleReplica struct {
	vars      *countingVariables
	coll      *countingCollectors
	handler   *AgentHandler
	variables variables.Store
}

type scaleFixture struct {
	dir         string
	d           *databasetest.Databases
	ctx         context.Context
	org         uuid.UUID
	creation    fleets.Creation
	publication uuid.UUID
	variableID  map[string]uuid.UUID
	groupIDs    []uuid.UUID
	collectors  []*scaleCollector
	replicas    []*scaleReplica
	// store reads and writes outside the poll, as the management API's
	collectorsStore collectors.Store
	variablesStore  variables.Store
	results         []string
	values          map[string]any
}

func newScaleFixture(b *testing.B, dir string) *scaleFixture {
	b.Helper()
	ctx := context.Background()
	d := databasetest.New(b, scaleDatabase)
	f := &scaleFixture{dir: dir, d: d, ctx: ctx, org: storetest.OrgA, variableID: map[string]uuid.UUID{},
		values: map[string]any{}}
	f.logf(b, "machine: %d CPUs, GOMAXPROCS %d, load average at start: %s", runtime.NumCPU(), runtime.GOMAXPROCS(0), loadAverage())
	f.values["load_average_start"] = loadAverage()

	now := time.Now().UTC()
	orgs := organizations.NewPostgres(d.Runtime)
	if result, _, err := orgs.Create(ctx, organizations.Organization{ID: f.org, Name: "scale", CreatedAt: now, UpdatedAt: now}); err != nil || result != organizations.Created {
		b.Fatalf("registering the organization: %v, %v", result, err)
	}
	result, creation, err := fleets.NewPostgres(d.Runtime).Create(ctx, fleets.NewFleet{
		ID: uuid.New(), OrganizationID: f.org, Label: "edge", Type: fleets.TypeServer, CreatedBy: storetest.Ana,
		RotateExpiresAt: storetest.Ptr(now.AddDate(1, 0, 0)), At: now,
	})
	if err != nil || result != fleets.Created {
		b.Fatalf("creating the fleet: %v, %v", result, err)
	}
	f.creation = creation
	f.variablesStore = variables.NewPostgres(d.Runtime)
	f.collectorsStore = collectors.NewPostgres(d.Runtime)

	// 150 variables, 6 variable groups and their overrides, through the store.
	for i := 1; i <= scaleVariables; i++ {
		name := scaleName(i)
		value, ok := scaleValues[name]
		if !ok {
			value = fmt.Sprintf("value-%d", i)
		}
		id := uuid.New()
		if result, _, err := f.variablesStore.CreateVariable(ctx, variables.NewVariable{
			ID: id, OrganizationID: f.org, Name: name, Value: value, Actor: storetest.Ana, At: now}); err != nil || result != variables.Created {
			b.Fatalf("creating %s: %v, %v", name, result, err)
		}
		f.variableID[name] = id
	}
	var variableGroups []uuid.UUID
	for k := 1; k <= 6; k++ {
		id := uuid.New()
		if result, _, err := f.variablesStore.CreateGroup(ctx, variables.NewGroup{
			ID: id, OrganizationID: f.org, Name: fmt.Sprintf("vg-%d", k), Actor: storetest.Ana, At: now}); err != nil || result != variables.Created {
			b.Fatalf("creating a variable group: %v, %v", result, err)
		}
		overridden := []int{1, 2, 3, 4, 5, 6}
		for i := 100; i < 120; i++ {
			overridden = append(overridden, i)
		}
		for _, i := range overridden {
			if _, _, err := f.variablesStore.SetOverride(ctx, f.org, id, f.variableID[scaleName(i)],
				fmt.Sprintf("ov-%d-%d", k, i), storetest.Ana, now); err != nil {
				b.Fatalf("setting an override: %v", err)
			}
		}
		if reset, err := f.variablesStore.ResetOverride(ctx, f.org, id, f.variableID[scaleName(6)], storetest.Ana, now); err != nil || !reset {
			b.Fatalf("resetting an override: %v, %v", reset, err)
		}
		variableGroups = append(variableGroups, id)
	}

	// The published version, assigned to every group; groups 1 to 10 bound
	// to variable groups 1 to 5.
	f.publication = uuid.New()
	if result, _, err := publications.NewPostgres(d.Runtime).Put(ctx, publications.Publication{
		ID: f.publication, OrganizationID: f.org, ConfigurationID: uuid.New(), ConfigurationName: "edge-gateway",
		VersionID: uuid.New(), VersionNumber: 3, YAML: scaleYAML, SHA256: sha(scaleYAML), PublishedAt: now, ReceivedAt: now,
		Collector: publications.Collector{Distribution: "otelcol-contrib", Version: "0.162.0"},
	}); err != nil || result != publications.Created {
		b.Fatalf("publishing: %v, %v", result, err)
	}
	groups := collectorgroups.NewPostgres(d.Runtime)
	for j := 1; j <= scaleGroups; j++ {
		var bound *uuid.UUID
		if j <= scaleBoundGroups {
			bound = &variableGroups[(j-1)%5]
		}
		if j == 1 {
			change := collectorgroups.Changes{AssignedPublicationID: collectorgroups.SetTo(f.publication)}
			if bound != nil {
				change.VariableGroupID = collectorgroups.SetTo(*bound)
			}
			if _, err := groups.Update(ctx, f.org, creation.Fleet.ID, creation.DefaultGroupID, change, now); err != nil {
				b.Fatalf("assigning the Default group: %v", err)
			}
			f.groupIDs = append(f.groupIDs, creation.DefaultGroupID)
			continue
		}
		g, err := groups.Create(ctx, collectorgroups.NewGroup{OrganizationID: f.org, FleetID: creation.Fleet.ID,
			Name: fmt.Sprintf("g-%02d", j), AssignedPublicationID: &f.publication, VariableGroupID: bound, At: now})
		if err != nil {
			b.Fatalf("creating a group: %v", err)
		}
		f.groupIDs = append(f.groupIDs, g.ID)
	}

	// 10,000 collectors by admin SQL, collector n in group 1 + n % 20, last
	// seen a heartbeat ago from the client's address; then 100 pinned to the
	// 6th variable group through the store (which stamps them).
	if _, err := d.Owner.Exec(ctx, `
		insert into telflo_opamp.collector_instances (id, organization_id, fleet_id, collector_group_id,
		  instance_uid, name, enrollment_token_id, reported_ip, last_seen_at, created_at, updated_at)
		select gen_random_uuid(), $1, $2, ($3::uuid[])[1 + n % $4], gen_random_uuid(), 'c-' || lpad(n::text, 5, '0'),
		       $5, $6, $7, $7, $7
		  from generate_series(0, $8 - 1) n`,
		f.org, creation.Fleet.ID, f.groupIDs, scaleGroups, creation.RotateTokenID, clientIP,
		now.Add(-30*time.Second), scaleCollectors); err != nil {
		b.Fatalf("inserting the collectors: %v", err)
	}
	rows, err := d.Owner.Query(ctx, `
		select id, instance_uid from telflo_opamp.collector_instances
		 where organization_id = $1 order by name`, f.org)
	if err != nil {
		b.Fatal(err)
	}
	for rows.Next() {
		c := &scaleCollector{}
		if err := rows.Scan(&c.id, &c.uid); err != nil {
			b.Fatal(err)
		}
		c.replica = len(f.collectors) % scaleReplicas
		f.collectors = append(f.collectors, c)
	}
	if rows.Close(); rows.Err() != nil || len(f.collectors) != scaleCollectors {
		b.Fatalf("read %d collectors: %v", len(f.collectors), rows.Err())
	}
	pinned := make([]uuid.UUID, 0, scalePinned)
	for _, c := range f.collectors[:scalePinned] {
		pinned = append(pinned, c.id)
	}
	if _, err := f.collectorsStore.BulkPin(ctx, f.org, creation.Fleet.ID, pinned, nil,
		&collectors.VariableGroupChange{ID: &variableGroups[5]}, now); err != nil {
		b.Fatalf("pinning the 6th variable group: %v", err)
	}

	f.replicas = f.newReplicas(b)
	pushes := f.syncAll(b)
	f.vacuum(b)
	summary := f.summary(b)
	if pushes != scaleCollectors || summary.Collectors != scaleCollectors || summary.InSync != scaleCollectors {
		b.Fatalf("the fixture: %d pushes, %+v; want every collector served and In sync", pushes, summary)
	}
	f.logf(b, "fixture: %d collectors in sync (%d pushes to get there)", summary.InSync, pushes)
	return f
}

// newReplicas builds the pods: each its own pool of scalePoolMaxConns
// connections as the server's role, its stores, YAML and variables caches,
// authenticator and guard, on the real clock.
func (f *scaleFixture) newReplicas(b *testing.B) []*scaleReplica {
	b.Helper()
	k, err := tokens.NewKeyring(keySetting, nil)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(k.Destroy)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	var out []*scaleReplica
	for range scaleReplicas {
		cfg, err := pgxpool.ParseConfig(f.d.RuntimeURL)
		if err != nil {
			b.Fatal(err)
		}
		cfg.MaxConns = scalePoolMaxConns
		pool, err := pgxpool.NewWithConfig(f.ctx, cfg)
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(pool.Close)
		vars := variables.NewPostgres(pool)
		r := &scaleReplica{variables: vars,
			vars: &countingVariables{Store: vars}, coll: &countingCollectors{Store: collectors.NewPostgres(pool)}}
		tokenStore := enrollmenttokens.NewPostgres(pool)
		if r.handler, err = NewAgentHandler(Options{
			Collectors: r.coll, Tokens: tokenStore, Variables: r.vars, Keyring: k,
			Authenticator: middleware.NewAuthenticator(k, tokenStore, time.Now, logger),
			Guard:         middleware.NewCollectorGuard(middleware.DefaultCollectorRequestsPerMinute, time.Now, logger),
			ServerURL:     serverURL, HeartbeatInterval: 30 * time.Second,
			Cache: NewYamlCache(), VariablesCache: NewVariablesCache(), Now: time.Now, Logger: logger,
		}); err != nil {
			b.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// scalePoll is what one poll did.
type scalePoll struct {
	at, latency, lag time.Duration
	replica          int
	followUp         bool
	status           int
	pushed           bool
	// fullState is an answer asking for the agent's full state.
	fullState bool
	// miss is a read of the variables (a cache miss), wrote a Report call.
	miss, wrote bool
}

type scalePollKey struct{}

func pollRecord(ctx context.Context) *scalePoll {
	r, _ := ctx.Value(scalePollKey{}).(*scalePoll)
	return r
}

// countingVariables counts the variables reads (the cache's misses) and
// marks the poll that made one.
type countingVariables struct {
	orgvariables.Store
	reads atomic.Int64
}

func (v *countingVariables) Variables(ctx context.Context, org uuid.UUID, group, pinned *uuid.UUID) (orgvariables.Resolved, error) {
	v.reads.Add(1)
	if r := pollRecord(ctx); r != nil {
		r.miss = true
	}
	return v.Store.Variables(ctx, org, group, pinned)
}

// countingCollectors counts the Report calls (the writes past the
// heartbeat), the error rows and the YAML reads, and marks the poll that
// made a Report call.
type countingCollectors struct {
	collectors.Store
	reports, errors, yamls atomic.Int64
}

func (c *countingCollectors) Report(ctx context.Context, r collectors.Report) (bool, error) {
	c.reports.Add(1)
	if p := pollRecord(ctx); p != nil {
		p.wrote = true
	}
	return c.Store.Report(ctx, r)
}

func (c *countingCollectors) RecordError(ctx context.Context, organizationID, fleetID, collectorID uuid.UUID, e collectors.ErrorReport) error {
	c.errors.Add(1)
	return c.Store.RecordError(ctx, organizationID, fleetID, collectorID, e)
}

func (c *countingCollectors) YAML(ctx context.Context, organizationID uuid.UUID, ref collectors.Ref) (collectors.YAMLResult, error) {
	c.yamls.Add(1)
	return c.Store.YAML(ctx, organizationID, ref)
}

// poll sends the collector's next message through its replica: its
// pushed configuration's APPLIED status with its effective configuration
// (the pushed bytes), as a supervisor reports one, when it has one to
// report, else a heartbeat (no description, no status), and records what
// happened. The latency is ServeHTTP's alone.
func (f *scaleFixture) poll(b *testing.B, c *scaleCollector, rec *scalePoll) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	msg := message(c.uid, c.seq, withoutDescription())
	if c.ack != nil {
		msg.RemoteConfigStatus = &protobufs.RemoteConfigStatus{LastRemoteConfigHash: c.ack,
			Status: protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED}
		msg.EffectiveConfig = &protobufs.EffectiveConfig{ConfigMap: &protobufs.AgentConfigMap{
			ConfigMap: map[string]*protobufs.AgentConfigObject{"": {Body: c.ackBody, ContentType: "text/yaml"}}}}
		rec.followUp = true
	}
	body, err := proto.Marshal(msg)
	if err != nil {
		b.Error(err)
		return
	}
	req := httptest.NewRequest(http.MethodPost, middleware.AgentRoute, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-protobuf")
	req = req.WithContext(middleware.WithPrincipal(context.WithValue(f.ctx, scalePollKey{}, rec), rotate(f.creation)))
	w := httptest.NewRecorder()
	r := f.replicas[c.replica]
	rec.replica = c.replica
	start := time.Now()
	r.handler.ServeHTTP(w, req)
	rec.latency = time.Since(start)
	rec.status = w.Code
	if w.Code != http.StatusOK {
		return
	}
	answer := &protobufs.ServerToAgent{}
	if err := proto.Unmarshal(w.Body.Bytes(), answer); err != nil {
		b.Error(err)
		return
	}
	rec.fullState = fullState(answer)
	if rc := answer.GetRemoteConfig(); rc != nil {
		rec.pushed = true
		c.ack = rc.GetConfigHash()
		c.ackBody = rc.GetConfig().GetConfigMap()["otelcol.yml"].GetBody()
	} else if rec.followUp {
		c.ack, c.ackBody = nil, nil
	}
}

// syncAll polls every collector until it is served and has reported what it
// was pushed APPLIED (scaleWorkers at once), and returns the pushes.
func (f *scaleFixture) syncAll(b *testing.B) int {
	b.Helper()
	polls := f.pollEach(b, func(c *scaleCollector, record func(*scalePoll)) {
		for {
			rec := &scalePoll{}
			f.poll(b, c, rec)
			record(rec)
			if rec.status != http.StatusOK || !rec.pushed {
				return
			}
			follow := &scalePoll{}
			f.poll(b, c, follow)
			record(follow)
			if follow.status != http.StatusOK || !follow.pushed {
				return
			}
		}
	})
	pushes := 0
	for _, p := range polls {
		if p.status != http.StatusOK {
			b.Fatalf("a poll answered %d", p.status)
		}
		if p.pushed {
			pushes++
		}
	}
	return pushes
}

// pollEach runs each(c) for every collector on scaleWorkers goroutines, as
// fast as they go, and returns the polls recorded.
func (f *scaleFixture) pollEach(b *testing.B, each func(c *scaleCollector, record func(*scalePoll))) []*scalePoll {
	b.Helper()
	var mu sync.Mutex
	var polls []*scalePoll
	record := func(p *scalePoll) {
		mu.Lock()
		polls = append(polls, p)
		mu.Unlock()
	}
	var next atomic.Int64
	var wg sync.WaitGroup
	for range scaleWorkers {
		wg.Go(func() {
			for i := next.Add(1) - 1; i < int64(len(f.collectors)); i = next.Add(1) - 1 {
				each(f.collectors[i], record)
			}
		})
	}
	wg.Wait()
	return polls
}

// vacuum is VACUUM ANALYZE of the tables the polls and the counts read.
func (f *scaleFixture) vacuum(b *testing.B) {
	b.Helper()
	if _, err := f.d.Admin.Exec(f.ctx, `vacuum analyze telflo_opamp.collector_instances, telflo_opamp.collector_groups,
		telflo_opamp.variables, telflo_opamp.variable_group_overrides, telflo_opamp.variable_groups,
		telflo_opamp.publication_variable_names, telflo_opamp.organizations`); err != nil {
		b.Fatal(err)
	}
}

func (f *scaleFixture) summary(b *testing.B) collectors.Summary {
	b.Helper()
	got, err := f.collectorsStore.SummariesForFleets(f.ctx, f.org, []uuid.UUID{f.creation.Fleet.ID}, time.Now())
	if err != nil {
		b.Fatal(err)
	}
	return got[f.creation.Fleet.ID]
}

// change sets the variable's value to a new one through the store, and
// times the write.
func (f *scaleFixture) change(b *testing.B, store variables.Store, name string, round int) time.Duration {
	b.Helper()
	took, err := f.tryChange(store, name, round)
	if err != nil {
		b.Fatal(err)
	}
	return took
}

func (f *scaleFixture) tryChange(store variables.Store, name string, round int) (time.Duration, error) {
	value := fmt.Sprintf("value-changed-%d", round)
	if known, ok := scaleValues[name]; ok {
		value = fmt.Sprintf("%s-changed-%d", known, round)
	}
	start := time.Now()
	if _, err := store.UpdateVariable(f.ctx, f.org, f.variableID[name], variables.VariableChanges{Value: &value},
		storetest.Ana, time.Now()); err != nil {
		return 0, fmt.Errorf("changing %s: %w", name, err)
	}
	return time.Since(start), nil
}

// The sub-benchmarks.

// fleetCounts times SummariesForFleets, a List page (50, the web's) and a
// Get, scaleRepeats times each, before and after a used value's change, and
// saves EXPLAIN (ANALYZE, BUFFERS) of each statement.
func (f *scaleFixture) fleetCounts(b *testing.B) {
	f.logf(b, "fleet counts: load average %s", loadAverage())
	f.explain(b, "before")
	before := f.timeReads(b)
	f.change(b, f.variablesStore, usedName, 0)
	after := f.timeReads(b)
	f.explain(b, "after")
	s := f.summary(b)
	f.logf(b, "after %s's change: %d In sync, %d Pending", usedName, s.InSync, s.Pending)
	f.values["counts_after_change"] = map[string]int{"in_sync": s.InSync, "pending": s.Pending}
	for _, op := range []string{"SummariesForFleets", "List (50)", "Get"} {
		f.table(b, "fleet counts: "+op+" before the change", before[op])
		f.table(b, "fleet counts: "+op+" after the change", after[op])
	}
}

func (f *scaleFixture) timeReads(b *testing.B) map[string][]time.Duration {
	b.Helper()
	out := map[string][]time.Duration{}
	fleet := f.creation.Fleet.ID
	for i := range scaleRepeats {
		start := time.Now()
		if _, err := f.collectorsStore.SummariesForFleets(f.ctx, f.org, []uuid.UUID{fleet}, time.Now()); err != nil {
			b.Fatal(err)
		}
		out["SummariesForFleets"] = append(out["SummariesForFleets"], time.Since(start))
		start = time.Now()
		if _, err := f.collectorsStore.List(f.ctx, f.org, fleet, collectors.ListOptions{
			Sort: collectors.SortCreated, Order: collectors.Ascending, Limit: 50}); err != nil {
			b.Fatal(err)
		}
		out["List (50)"] = append(out["List (50)"], time.Since(start))
		c := f.collectors[(i*197)%len(f.collectors)]
		start = time.Now()
		if _, err := f.collectorsStore.Get(f.ctx, f.org, fleet, c.id); err != nil {
			b.Fatal(err)
		}
		out["Get"] = append(out["Get"], time.Since(start))
	}
	return out
}

// statementCapture keeps the statements a pool's connections ran.
type statementCapture struct {
	mu         sync.Mutex
	statements []capturedStatement
}

type capturedStatement struct {
	sql  string
	args []any
}

func (s *statementCapture) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	s.mu.Lock()
	s.statements = append(s.statements, capturedStatement{sql: d.SQL, args: d.Args})
	s.mu.Unlock()
	return ctx
}

func (s *statementCapture) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// longest is the longest statement captured since the last call (the
// store's read, past any begin or commit), and forgets them all.
func (s *statementCapture) longest() capturedStatement {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out capturedStatement
	for _, st := range s.statements {
		if len(st.sql) > len(out.sql) {
			out = st
		}
	}
	s.statements = nil
	return out
}

// explain runs each read once on a pool that captures its statement, then
// EXPLAIN (ANALYZE, BUFFERS) of that statement with its arguments, saved in
// the check's folder.
func (f *scaleFixture) explain(b *testing.B, when string) {
	b.Helper()
	capture := &statementCapture{}
	cfg, err := pgxpool.ParseConfig(f.d.RuntimeURL)
	if err != nil {
		b.Fatal(err)
	}
	cfg.ConnConfig.Tracer = capture
	pool, err := pgxpool.NewWithConfig(f.ctx, cfg)
	if err != nil {
		b.Fatal(err)
	}
	defer pool.Close()
	store := collectors.NewPostgres(pool)
	fleet := f.creation.Fleet.ID
	reads := []struct {
		name string
		run  func() error
	}{
		{"summaries", func() error {
			_, err := store.SummariesForFleets(f.ctx, f.org, []uuid.UUID{fleet}, time.Now())
			return err
		}},
		{"list", func() error {
			_, err := store.List(f.ctx, f.org, fleet, collectors.ListOptions{Sort: collectors.SortCreated, Order: collectors.Ascending, Limit: 50})
			return err
		}},
		{"get", func() error {
			_, err := store.Get(f.ctx, f.org, fleet, f.collectors[0].id)
			return err
		}},
	}
	for _, read := range reads {
		capture.longest()
		if err := read.run(); err != nil {
			b.Fatal(err)
		}
		st := capture.longest()
		rows, err := pool.Query(f.ctx, "explain (analyze, buffers) "+st.sql, st.args...)
		if err != nil {
			b.Fatalf("explaining %s: %v", read.name, err)
		}
		var plan strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				b.Fatal(err)
			}
			plan.WriteString(line + "\n")
		}
		rows.Close()
		if rows.Err() != nil {
			b.Fatal(rows.Err())
		}
		path := filepath.Join(f.dir, fmt.Sprintf("explain-%s-%s.txt", read.name, when))
		if err := os.WriteFile(path, []byte(st.sql+"\n\n"+plan.String()), 0o644); err != nil {
			b.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(plan.String()), "\n")
		f.logf(b, "EXPLAIN %s %s: %s (%s)", read.name, when, lines[len(lines)-1], path)
	}
}

// save times scaleRepeats changes of a used value (UpdateVariable, the write
// transaction).
func (f *scaleFixture) save(b *testing.B) {
	f.logf(b, "save: load average %s", loadAverage())
	var times []time.Duration
	for i := range scaleRepeats {
		times = append(times, f.change(b, f.variablesStore, usedName, i+1))
	}
	f.table(b, "save: UpdateVariable of "+usedName, times)
}

// unusedValueChanged resynchronizes the fixture (the saves left about half
// of it Pending), changes a value no text uses, and polls every collector
// once: pushes and writes past the heartbeat are asserted 0 (counts, not
// times), every collector stays In sync.
func (f *scaleFixture) unusedValueChanged(b *testing.B) {
	f.syncAll(b)
	f.vacuum(b)
	if s := f.summary(b); s.InSync != scaleCollectors {
		b.Fatalf("resynchronized: %+v", s)
	}
	f.logf(b, "unused value changed: load average %s", loadAverage())
	f.change(b, f.variablesStore, unusedName, 1)
	reports, errs, reads := f.counters()
	polls := f.pollEach(b, func(c *scaleCollector, record func(*scalePoll)) {
		rec := &scalePoll{}
		f.poll(b, c, rec)
		record(rec)
	})
	pushes := 0
	for _, p := range polls {
		if p.status != http.StatusOK {
			b.Fatalf("a poll answered %d", p.status)
		}
		if p.pushed {
			pushes++
		}
	}
	r2, e2, v2 := f.counters()
	s := f.summary(b)
	f.logf(b, "unused value changed: %d polls, %d pushes, %d Report writes, %d error rows, %d variable reads (misses, both replicas), %d In sync",
		len(polls), pushes, r2-reports, e2-errs, v2-reads, s.InSync)
	f.values["unused_value_changed"] = map[string]int64{"polls": int64(len(polls)), "pushes": int64(pushes),
		"reports": r2 - reports, "errors": e2 - errs, "variable_reads": v2 - reads, "in_sync": int64(s.InSync)}
	if pushes != 0 || r2 != reports || e2 != errs || s.InSync != scaleCollectors {
		b.Errorf("an unused value's change: %d pushes, %d writes, %d error rows, %d In sync; want 0, 0, 0, %d",
			pushes, r2-reports, e2-errs, s.InSync, scaleCollectors)
	}
}

func (f *scaleFixture) counters() (reports, errs, reads int64) {
	for _, r := range f.replicas {
		reports += r.coll.reports.Load()
		errs += r.coll.errors.Load()
		reads += r.vars.reads.Load()
	}
	return reports, errs, reads
}

// pollDuringAChange polls at scalePollsPerSec across the replicas (each
// collector on its own replica, as a kept-alive connection is) for scaleRun,
// scaleWorkers goroutines in all, with herdName's value changed at
// scaleChangeAt: every collector's text changes (the push herd). A pushed
// collector reports APPLIED scaleFollowUp later, as a supervisor does. It
// records the latency per window, of the steady polls before the change, of
// the polls that missed the cache, pushed or reported APPLIED, the full-state
// asks, the process's CPU per poll dispatched before the change (the load
// generator's included), and a verdict: saturated when a poll started more
// than scaleLagLimit after it was due. Every scheduled poll runs (the
// queues drain) and is due before the run's end, so one that started after
// the run's end + scaleLagLimit is one of those.
func (f *scaleFixture) pollDuringAChange(b *testing.B) {
	load := loadAverage()
	f.logf(b, "poll during a change: load average %s", load)
	f.values["load_average_poll"] = load
	order := slices.Clone(f.collectors)
	rand.New(rand.NewPCG(8, 20)).Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })

	type job struct {
		c        *scaleCollector
		due      time.Time
		followUp bool
	}
	queues := make([]chan job, scaleReplicas)
	for i := range queues {
		queues[i] = make(chan job, 4*scaleCollectors)
	}
	reports, errs, reads := f.counters()
	var yamls int64
	readsBefore := make([]int64, scaleReplicas)
	for i, r := range f.replicas {
		yamls += r.coll.yamls.Load()
		readsBefore[i] = r.vars.reads.Load()
	}

	var mu sync.Mutex
	var polls []*scalePoll
	// followUps counts the APPLIED reports scheduled; none is scheduled
	// once closed, so the queues close after the last.
	var followUps sync.WaitGroup
	closed := false
	followUpsScheduled := 0
	// A heartbeat interval since the last pass, so every poll writes its
	// heartbeat (HeartbeatCoalesce), as a steady fleet's do.
	time.Sleep(30 * time.Second)
	start := time.Now()
	end := start.Add(scaleRun)
	var workers sync.WaitGroup
	for w := range scaleWorkers {
		queue := queues[w%scaleReplicas]
		workers.Go(func() {
			for j := range queue {
				rec := &scalePoll{at: time.Since(start), lag: time.Since(j.due)}
				f.poll(b, j.c, rec)
				mu.Lock()
				polls = append(polls, rec)
				schedule := rec.pushed && !closed && time.Now().Add(scaleFollowUp).Before(end)
				if schedule {
					followUps.Add(1)
					followUpsScheduled++
				}
				mu.Unlock()
				if schedule {
					due := time.Now().Add(scaleFollowUp)
					time.AfterFunc(scaleFollowUp, func() {
						defer followUps.Done()
						queues[j.c.replica] <- job{c: j.c, due: due, followUp: true}
					})
				}
			}
		})
	}

	var changeTook time.Duration
	changed := make(chan struct{})
	go func() {
		defer close(changed)
		time.Sleep(time.Until(start.Add(scaleChangeAt)))
		var err error
		if changeTook, err = f.tryChange(f.replicas[0].variables, herdName, 1); err != nil {
			b.Error(err)
		}
	}()
	var steadyCPU [2]time.Duration
	steadyCPU[0] = processCPU()
	interval := time.Second / scalePollsPerSec
	cpuTaken := false
	// heartbeats is the polls the loop dispatched; dispatchedBeforeChange
	// those due before the change, over which steadyCPU is taken.
	heartbeats, dispatchedBeforeChange := 0, 0
	for i := 0; ; i++ {
		due := start.Add(time.Duration(i) * interval)
		if !due.Before(end) {
			heartbeats = i
			break
		}
		if !cpuTaken && !due.Before(start.Add(scaleChangeAt)) {
			steadyCPU[1], cpuTaken, dispatchedBeforeChange = processCPU(), true, i
		}
		time.Sleep(time.Until(due))
		c := order[i%len(order)]
		queues[c.replica] <- job{c: c, due: due}
	}
	<-changed
	mu.Lock()
	closed = true
	mu.Unlock()
	followUps.Wait()
	for _, q := range queues {
		close(q)
	}
	workers.Wait()

	r2, e2, v2 := f.counters()
	var y2 int64
	missesPerReplica := make([]int64, scaleReplicas)
	for i, r := range f.replicas {
		y2 += r.coll.yamls.Load()
		missesPerReplica[i] = r.vars.reads.Load() - readsBefore[i]
	}
	pushes, failed, fullStates, served := 0, 0, 0, 0
	var maxLag time.Duration
	var steady, misses, pushed, applied, all []time.Duration
	for _, p := range polls {
		all = append(all, p.latency)
		maxLag = max(maxLag, p.lag)
		if p.at < scaleRun+scaleLagLimit {
			served++
		}
		if p.fullState {
			fullStates++
		}
		if p.status != http.StatusOK {
			failed++
		}
		switch {
		case p.miss:
			misses = append(misses, p.latency)
		case p.pushed:
			pushed = append(pushed, p.latency)
		case p.followUp:
			applied = append(applied, p.latency)
		case p.at < scaleChangeAt && !p.wrote:
			steady = append(steady, p.latency)
		}
		if p.pushed {
			pushes++
		}
	}
	scheduled := heartbeats + followUpsScheduled
	saturated := maxLag > scaleLagLimit
	verdict := fmt.Sprintf("not saturated: every poll started within %s of its due time, the most late %.0f ms; "+
		"the machine's load still shows in these latencies (load average at the start: %s)", scaleLagLimit, ms(maxLag), load)
	if saturated {
		verdict = fmt.Sprintf("SATURATED: %d of %d scheduled polls started by the run's end + %s, the most late %.0f ms; "+
			"the load generator fell behind, so these latencies are the machine's, not the server's", served, scheduled, scaleLagLimit, ms(maxLag))
	}
	f.logf(b, "poll during a change: %s", verdict)
	f.values["poll_saturated"] = saturated
	f.values["poll_verdict"] = verdict
	f.logf(b, "poll during a change: %d polls in %s (%d scheduled, %d answered other than 200), the change took %.2f ms; %d pushes, %d Report writes, %d full-state asks, %d error rows, %d YAML reads, %d variable reads (misses; per replica %v)",
		len(polls), scaleRun, scheduled, failed, ms(changeTook), pushes, r2-reports, fullStates, e2-errs, y2-yamls, v2-reads, missesPerReplica)
	f.values["poll_counts"] = map[string]any{"polls": len(polls), "scheduled": scheduled, "non_200": failed, "pushes": pushes,
		"reports": r2 - reports, "full_state_asks": fullStates, "errors": e2 - errs, "yaml_reads": y2 - yamls,
		"variable_reads": v2 - reads, "change_ms": ms(changeTook), "max_lag_ms": ms(maxLag)}
	f.table(b, "poll: steady polls before the change", steady)
	f.table(b, "poll: polls that missed the variables cache", misses)
	f.table(b, "poll: polls that pushed (hits)", pushed)
	f.table(b, "poll: polls reporting APPLIED", applied)
	f.table(b, "poll: every poll", all)
	if dispatchedBeforeChange > 0 {
		perPoll := (steadyCPU[1] - steadyCPU[0]) / time.Duration(dispatchedBeforeChange)
		f.logf(b, "poll: process CPU per poll dispatched before the change (%d polls; load generator included): %s",
			dispatchedBeforeChange, micro(perPoll))
		f.values["process_cpu_per_steady_poll_us"] = float64(perPoll.Nanoseconds()) / 1e3
	}
	f.windows(b, polls)
	s := f.summary(b)
	f.logf(b, "poll: after the run, %d In sync, %d Pending (a push whose APPLIED came after the run reads Pending)", s.InSync, s.Pending)
	f.values["poll_after"] = map[string]int{"in_sync": s.InSync, "pending": s.Pending}
	if failed > 0 {
		b.Errorf("%d polls answered other than 200", failed)
	}
}

// windows logs each scaleWindow's polls by the time they started: count,
// pushes, APPLIED reports, Report writes, full-state asks, misses, the
// latency's percentiles and the most late start; the polls that started
// after the run (a backlog) get a row of their own.
func (f *scaleFixture) windows(b *testing.B, polls []*scalePoll) {
	n := int(scaleRun / scaleWindow)
	type window struct {
		latencies                                  []time.Duration
		lag                                        time.Duration
		pushes, misses, writes, applied, fullState int
	}
	ws := make([]window, n+1)
	for _, p := range polls {
		i := min(int(p.at/scaleWindow), n)
		ws[i].latencies = append(ws[i].latencies, p.latency)
		ws[i].lag = max(ws[i].lag, p.lag)
		if p.pushed {
			ws[i].pushes++
		}
		if p.miss {
			ws[i].misses++
		}
		if p.wrote {
			ws[i].writes++
		}
		if p.followUp {
			ws[i].applied++
		}
		if p.fullState {
			ws[i].fullState++
		}
	}
	lines := []string{"| window (s) | polls | pushes | APPLIED reports | Report writes | full-state asks | misses | p50 (ms) | p95 (ms) | p99 (ms) | max (ms) | max lag (ms) |",
		"|---|---|---|---|---|---|---|---|---|---|---|---|"}
	var rows []map[string]any
	seconds := scaleWindow.Seconds()
	for i, w := range ws {
		label := fmt.Sprintf("%g–%g", float64(i)*seconds, float64(i+1)*seconds)
		if i == n {
			if len(w.latencies) == 0 {
				continue
			}
			label = "after the run"
		}
		p := percentiles(w.latencies)
		lines = append(lines, fmt.Sprintf("| %s | %d | %d | %d | %d | %d | %d | %.2f | %.2f | %.2f | %.2f | %.1f |",
			label, len(w.latencies), w.pushes, w.applied, w.writes, w.fullState, w.misses, p["p50"], p["p95"], p["p99"], p["max"], ms(w.lag)))
		rows = append(rows, map[string]any{"window": label, "polls": len(w.latencies), "pushes": w.pushes,
			"applied": w.applied, "writes": w.writes, "full_state_asks": w.fullState, "misses": w.misses,
			"latency_ms": p, "max_lag_ms": ms(w.lag)})
	}
	f.values["poll_windows"] = rows
	f.logf(b, "poll per %s window (the change at %s):\n%s", scaleWindow, scaleChangeAt, strings.Join(lines, "\n"))
}

// fillInCPU measures a steady poll's fill-in, RenderedLength, Render and
// the SHA-256 of the text, scaleFillInRounds times on scaleYAML and on a
// 256 KiB text, with one collector's resolved values (150 variables):
// process CPU and wall time per poll, and allocations.
func (f *scaleFixture) fillInCPU(b *testing.B) {
	f.logf(b, "fill-in CPU: load average %s", loadAverage())
	resolved, err := f.variablesStore.Variables(f.ctx, f.org, nil, nil)
	if err != nil {
		b.Fatal(err)
	}
	var large strings.Builder
	for large.Len() < 256<<10 {
		large.WriteString(scaleYAML)
	}
	measures := map[string]any{}
	for _, text := range []struct {
		name string
		yaml string
	}{{"realistic", scaleYAML}, {"256 KiB", large.String()}, {"realistic, no reference", strings.ReplaceAll(scaleYAML, "${org:", "${env:")}} {
		fillIn := func() {
			n, names := orgvariables.RenderedLength(text.yaml, resolved.Values)
			if n > orgvariables.MaxRenderedBytes || len(names) == 0 && text.name != "realistic, no reference" {
				b.Fatalf("%s: %d bytes, %d names", text.name, n, len(names))
			}
			rendered, _ := orgvariables.Render(text.yaml, resolved.Values)
			_ = sha256.Sum256([]byte(rendered))
		}
		rounds := scaleFillInRounds
		if len(text.yaml) > 64<<10 {
			rounds = scaleFillInRounds / 50
		}
		fillIn()
		runtime.GC()
		var m0, m1 runtime.MemStats
		runtime.ReadMemStats(&m0)
		cpu0, wall0 := processCPU(), time.Now()
		for range rounds {
			fillIn()
		}
		cpu, wall := processCPU()-cpu0, time.Since(wall0)
		runtime.ReadMemStats(&m1)
		per := map[string]float64{
			"bytes":         float64(len(text.yaml)),
			"cpu_us":        float64(cpu.Nanoseconds()) / float64(rounds) / 1e3,
			"wall_us":       float64(wall.Nanoseconds()) / float64(rounds) / 1e3,
			"alloc_bytes":   float64(m1.TotalAlloc-m0.TotalAlloc) / float64(rounds),
			"allocs":        float64(m1.Mallocs-m0.Mallocs) / float64(rounds),
			"polls_per_cpu": 1e6 / (float64(cpu.Nanoseconds()) / float64(rounds) / 1e3),
		}
		measures[text.name] = per
		f.logf(b, "fill-in CPU, %s text (%d bytes): %.1f µs CPU (the process's, the GC's on other threads included) and %.1f µs wall per poll, %.0f B and %.0f allocations per poll (%d rounds)",
			text.name, len(text.yaml), per["cpu_us"], per["wall_us"], per["alloc_bytes"], per["allocs"], rounds)
	}
	f.values["fill_in"] = measures
}

// Reporting.

func (f *scaleFixture) logf(b *testing.B, format string, args ...any) {
	b.Helper()
	line := fmt.Sprintf(format, args...)
	f.results = append(f.results, line)
	b.Log(line)
}

// table logs the durations' p50, p95, p99 and max in milliseconds.
func (f *scaleFixture) table(b *testing.B, name string, ds []time.Duration) {
	b.Helper()
	p := percentiles(ds)
	f.values[name] = p
	f.logf(b, "%s: n %d, p50 %.2f ms, p95 %.2f ms, p99 %.2f ms, max %.2f ms", name, len(ds), p["p50"], p["p95"], p["p99"], p["max"])
}

func (f *scaleFixture) writeResults(b *testing.B) {
	b.Helper()
	f.values["load_average_end"] = loadAverage()
	stamp := time.Now().UTC().Format("20060102T150405Z")
	if err := os.WriteFile(filepath.Join(f.dir, "results-"+stamp+".txt"), []byte(strings.Join(f.results, "\n")+"\n"), 0o644); err != nil {
		b.Fatal(err)
	}
	encoded, err := json.MarshalIndent(f.values, "", "  ")
	if err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "results-"+stamp+".json"), encoded, 0o644); err != nil {
		b.Fatal(err)
	}
}

// percentiles is the nearest-rank p50, p95, p99 and the max, in
// milliseconds.
func percentiles(ds []time.Duration) map[string]float64 {
	out := map[string]float64{"n": float64(len(ds))}
	if len(ds) == 0 {
		return out
	}
	sorted := slices.Clone(ds)
	slices.Sort(sorted)
	rank := func(q float64) time.Duration {
		return sorted[max(0, int(math.Ceil(q*float64(len(sorted))))-1)]
	}
	out["p50"], out["p95"], out["p99"], out["max"] = ms(rank(0.50)), ms(rank(0.95)), ms(rank(0.99)), ms(sorted[len(sorted)-1])
	return out
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1e3 }

func micro(d time.Duration) string { return fmt.Sprintf("%.1f µs", float64(d.Nanoseconds())/1e3) }

// processCPU is the process's user and system CPU time so far.
func processCPU() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// loadAverage is the machine's 1-, 5- and 15-minute load averages, as
// uptime prints them, or "unknown".
func loadAverage() string {
	if raw, err := os.ReadFile("/proc/loadavg"); err == nil {
		return strings.Join(strings.Fields(string(raw))[:3], " ")
	}
	out, err := uptimeOutput()
	if err != nil {
		return "unknown"
	}
	if i := strings.Index(out, "load average"); i >= 0 {
		return strings.TrimSpace(strings.TrimLeft(out[i+len("load average"):], "s:"))
	}
	return "unknown"
}

func uptimeOutput() (string, error) {
	out, err := exec.Command("uptime").Output()
	return string(out), err
}
