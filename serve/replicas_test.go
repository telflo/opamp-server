package serve

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"

	"github.com/telflo/opamp-server/agent"
	"github.com/telflo/opamp-server/management"
	"github.com/telflo/opamp-server/middleware"
	"github.com/telflo/opamp-server/store/collectors"
	"github.com/telflo/opamp-server/store/database/databasetest"
)

// Told to stop, serve answers /ready 503 at once and keeps both listeners
// serving for OPAMP_SHUTDOWN_DRAIN_SECONDS, so the load balancer's
// readiness checks see the 503 before the port closes (Shutdown alone
// closes it at once, before they can); only once the drain has passed (its
// timer, the test's channel here) do the listeners stop, serve exiting 0.
func TestServeDrainsBeforeItsListenersStop(t *testing.T) {
	d := testDB(t)
	env := envOf(map[string]string{
		DatabaseURLVariable: d.RuntimeURL, management.HostVariable: "127.0.0.1", management.PortVariable: "0",
		management.SecretVariable: testSecret, EncryptionKeyVariable: testEncryptionKey,
		agent.HostVariable: "127.0.0.1", agent.PortVariable: "0", agent.ServerURLVariable: testServerURL,
		ShutdownDrainVariable: "3",
	})
	asked := make(chan time.Duration, 1)
	drained := make(chan time.Time)
	var built *replica
	adjust := func(r *replica) {
		built = r
		r.drainTimer = func(d time.Duration) <-chan time.Time {
			asked <- d
			return drained
		}
	}
	log := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exited := make(chan int, 1)
	go func() {
		exited <- serveWith(ctx, RoleAll, env, unsetNothing, slog.New(slog.NewJSONHandler(log, nil)), (&destroyRecorder{}).destroy, adjust)
	}()
	agent := "http://" + waitForLine(t, log, "serving the agent endpoint")["address"].(string)
	cancel()

	var wait time.Duration
	select {
	case wait = <-asked:
	case <-time.After(time.Minute):
		t.Fatal("serve never waited on the drain once told to stop")
	}
	if wait != 3*time.Second {
		t.Fatalf("the drain waited %v; want the setting's 3 s", wait)
	}
	// Draining: /ready answers 503 over the network, through the port still
	// open, and serve hasn't stopped.
	if status, _ := over(t, http.MethodGet, agent+"/ready", "", nil); status != http.StatusServiceUnavailable {
		t.Fatalf("/ready while draining: %d; want 503 through the open port", status)
	}
	if probe(built.agent, http.MethodGet, "/ready").Code != http.StatusServiceUnavailable {
		t.Fatal("/ready while draining isn't 503")
	}
	select {
	case code := <-exited:
		t.Fatalf("serve exited %d while draining", code)
	default:
	}
	close(drained)
	select {
	case code := <-exited:
		if code != 0 || !strings.Contains(log.String(), `"msg":"stopped"`) {
			t.Fatalf("serve exited %d; log %s", code, log.String())
		}
	case <-time.After(ShutdownTimeout + 30*time.Second):
		t.Fatal("serve didn't stop once the drain had passed")
	}
	if conn, err := net.Dial("tcp", strings.TrimPrefix(agent, "http://")); err == nil {
		_ = conn.Close()
		t.Fatal("the agent port still accepts once serve stopped")
	}
}

// Two replicas' cleanups run at once on one database: the one that takes
// the advisory lock deletes the expired error rows, the other says it
// didn't hold the lock and deletes nothing. A's run is held mid-delete by
// a row a third session locks, so B's comes while A holds the lock.
func TestTheCleanupRunsOnOneReplicaOnly(t *testing.T) {
	d := testDB(t)
	keys := testKeys(t)
	settings := replicaSettings(t, keys)
	clock := newTestClock()
	logA, logB := &syncBuffer{}, &syncBuffer{}
	a := testReplica(t, settings, postgresStores(d.Runtime), d.Runtime.Ping, clock, logA)
	b := testReplica(t, settings, postgresStores(d.Runtime), d.Runtime.Ping, clock, logB)
	f := fleetThrough(t, a)
	if rec, _ := pollThrough(t, a.agent, f.created.Tokens.Default.Bearer, agentMessage(t, uuid.New(), 1, f.created.DefaultGroup.ID)); rec.Code != http.StatusOK {
		t.Fatalf("enrolling: %d", rec.Code)
	}
	ctx := context.Background()
	old := clock.Now().Add(-collectors.ErrorRetention - time.Hour)
	if _, err := d.Owner.Exec(ctx, `
		insert into opamp_server.collector_errors (id, organization_id, collector_instance_id, reported_at, message)
		select gen_random_uuid(), organization_id, id, $1, 'old' from opamp_server.collector_instances, generate_series(1, 7)`,
		old); err != nil {
		t.Fatal(err)
	}
	blocker, err := d.Owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(ctx) }()
	if _, err := blocker.Exec(ctx, `select 1 from opamp_server.collector_errors limit 1 for update`); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.cleanup.once(ctx)
	}()
	waitForLockWaiters(t, d, 1)
	b.cleanup.once(ctx)
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	<-done
	linesA, linesB := logLines(t, logA.String()), logLines(t, logB.String())
	last := func(lines []map[string]any) map[string]any { return lines[len(lines)-1] }
	if len(linesB) == 0 || last(linesB)["held"] != false || !strings.Contains(last(linesB)["msg"].(string), "another replica holds its lock") {
		t.Fatalf("B's cleanup line: %v", linesB)
	}
	if len(linesA) == 0 || last(linesA)["held"] != true || last(linesA)["deleted"] != float64(7) {
		t.Fatalf("A's cleanup line: %v", linesA)
	}
	var left int
	if err := d.Admin.QueryRow(ctx, `select count(*) from opamp_server.collector_errors`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("%d error rows left, %v", left, err)
	}
}

// waitForLockWaiters waits until n sessions of the test database wait on a
// lock, or fails after a generous hang guard.
func waitForLockWaiters(t *testing.T, d *databasetest.Databases, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for {
		var waiting int
		if err := d.Admin.QueryRow(ctx, `select count(*) from pg_stat_activity where datname = $1 and wait_event_type = 'Lock'`,
			d.Name).Scan(&waiting); err != nil {
			t.Fatalf("waiting for %d sessions to wait on a lock: %v", n, err)
		}
		if waiting >= n {
			return
		}
	}
}

// gate holds the first POST that reaches it until release is closed.
type gate struct {
	next    http.Handler
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (g *gate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost && g.armed.CompareAndSwap(true, false) {
		close(g.entered)
		<-g.release
	}
	g.next.ServeHTTP(w, r)
}

// over sends one request to a listener over HTTP and answers its status and
// body.
func over(t *testing.T, method, target, authorization string, payload []byte) (int, []byte) {
	t.Helper()
	request, err := http.NewRequest(method, target, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	if payload != nil {
		request.Header.Set("Content-Type", jsonType)
	}
	response, err := (&http.Client{Transport: &http.Transport{Proxy: nil}}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	reply, _ := io.ReadAll(response.Body)
	return response.StatusCode, reply
}

// A graceful stop with a poll in flight, through the whole of serve on a
// real database. A collector's poll (its default token's hand-off) is held
// inside the agent listener; serve is told to stop: /ready answers 503 at
// once and the agent port stops taking connections, while the poll, let go,
// completes: it reads the database (the pool still open) and mints the
// handed bearer (the keys not yet destroyed), and is answered 200. Then
// serve stops, exit code 0, having run the cleanup at start, and destroys
// the keys once, after.
func TestAGracefulStopCompletesAPollInFlight(t *testing.T) {
	d := testDB(t)
	env := envOf(map[string]string{
		DatabaseURLVariable: d.RuntimeURL, management.HostVariable: "127.0.0.1", management.PortVariable: "0",
		management.SecretVariable: testSecret, EncryptionKeyVariable: testEncryptionKey,
		agent.HostVariable: "127.0.0.1", agent.PortVariable: "0", agent.ServerURLVariable: testServerURL,
		ShutdownDrainVariable: "0", // no drain: the listeners stop at once
	})
	var built *replica
	var held *gate
	adjust := func(r *replica) {
		built = r
		held = &gate{next: r.agent, entered: make(chan struct{}), release: make(chan struct{})}
		r.agent = held
		r.drainTimer = func(time.Duration) <-chan time.Time {
			t.Error("a drain of 0 waited on the drain's timer")
			return nil
		}
	}
	recorder := &destroyRecorder{}
	log := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exited := make(chan int, 1)
	go func() {
		exited <- serveWith(ctx, RoleAll, env, unsetNothing, slog.New(slog.NewJSONHandler(log, nil)), recorder.destroy, adjust)
	}()
	agent := "http://" + waitForLine(t, log, "serving the agent endpoint")["address"].(string)
	management := "http://" + waitForLine(t, log, "serving the management API")["address"].(string)
	waitForLine(t, log, "the error history's cleanup ran")
	if status, _ := over(t, http.MethodGet, agent+"/ready", "", nil); status != http.StatusOK {
		t.Fatalf("/ready while serving: %d", status)
	}

	secret := "Bearer " + testSecret
	organization := uuid.NewString()
	if status, reply := over(t, http.MethodPost, management+organizationsPath, secret,
		body{"id": organization, "name": "an organization"}.bytes()); status != http.StatusCreated {
		t.Fatalf("registering: %d %s", status, reply)
	}
	status, reply := over(t, http.MethodPost, management+fleetsTarget(organization), secret, fleetBody("", labelMarker, "server").bytes())
	var c created
	if status != http.StatusCreated {
		t.Fatalf("creating a fleet: %d %s", status, reply)
	}
	decodeInto(t, reply, &c)

	type answered struct {
		status int
		body   []byte
	}
	replied := make(chan answered, 1)
	held.armed.Store(true)
	go func() {
		status, reply := over(t, http.MethodPost, agent+middleware.AgentRoute, "Bearer "+c.Tokens.Default.Bearer, agentMessage(t, uuid.New(), 1, c.DefaultGroup.ID))
		replied <- answered{status, reply}
	}()
	<-held.entered
	cancel()

	// Bounded by attempts: /ready turns 503 and the port stops accepting.
	address := strings.TrimPrefix(agent, "http://")
	for attempt := 0; ; attempt++ {
		if attempt == 1000 {
			t.Fatal("/ready never turned 503, or the port kept accepting, once serve was told to stop")
		}
		readyNow := probe(built.agent, http.MethodGet, "/ready").Code
		conn, err := net.Dial("tcp", address)
		if err == nil {
			_ = conn.Close()
		}
		if readyNow == http.StatusServiceUnavailable && err != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case code := <-exited:
		t.Fatalf("serve exited %d with a poll in flight", code)
	default:
	}
	recorder.mu.Lock()
	early := len(recorder.destroyed)
	recorder.mu.Unlock()
	if early != 0 {
		t.Fatal("the keys were destroyed with a poll in flight")
	}
	close(held.release)
	got := <-replied
	answer := &protobufs.ServerToAgent{}
	if got.status != http.StatusOK || proto.Unmarshal(got.body, answer) != nil || pushedBearer(answer) == "" {
		t.Fatalf("the poll in flight: %d %q", got.status, got.body)
	}
	select {
	case code := <-exited:
		if code != 0 || !strings.Contains(log.String(), `"msg":"stopped"`) {
			t.Fatalf("serve exited %d; log %s", code, log.String())
		}
	case <-time.After(ShutdownTimeout + 30*time.Second):
		t.Fatal("serve didn't stop once the poll was answered")
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.destroyed) != 1 {
		t.Fatalf("the keys were destroyed %d times", len(recorder.destroyed))
	}
}

// tcpProxy forwards connections to a database, until it is told the
// database is down (every connection closed, new ones refused) or hangs
// (new connections accepted and never answered).
type tcpProxy struct {
	listener net.Listener
	target   string

	mu    sync.Mutex
	mode  string // "up", "down", "hang"
	conns []net.Conn
	// hung counts the connections accepted and left unanswered.
	hung int
}

func newTCPProxy(t *testing.T, target string) *tcpProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &tcpProxy{listener: listener, target: target, mode: "up"}
	t.Cleanup(func() {
		_ = listener.Close()
		p.set("down")
	})
	go p.accept()
	return p
}

func (p *tcpProxy) accept() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		mode := p.mode
		if mode == "down" {
			p.mu.Unlock()
			_ = client.Close()
			continue
		}
		p.conns = append(p.conns, client)
		p.mu.Unlock()
		if mode == "hang" {
			p.mu.Lock()
			p.hung++
			p.mu.Unlock()
			continue
		}
		server, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = client.Close()
			continue
		}
		p.mu.Lock()
		p.conns = append(p.conns, server)
		p.mu.Unlock()
		go func() { _, _ = io.Copy(server, client); _ = server.Close() }()
		go func() { _, _ = io.Copy(client, server); _ = client.Close() }()
	}
}

// set changes the mode; leaving "up", or "hang", closes every connection
// open.
func (p *tcpProxy) set(mode string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	previous := p.mode
	p.mode = mode
	if mode != "up" || previous == "hang" {
		for _, c := range p.conns {
			_ = c.Close()
		}
		p.conns = nil
	}
}

// On a real database behind a proxy, /ready answers 503 while serve
// starts, 200 once serving, 503 while the database is down (connections
// refused) or hangs (a ping bounded by ReadyTimeout), 200 once it is back,
// and 503 once draining.
func TestReadinessFollowsARealDatabaseOutage(t *testing.T) {
	d := testDB(t)
	runtime, err := url.Parse(d.RuntimeURL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := newTCPProxy(t, runtime.Host)
	runtime.Host = proxy.listener.Addr().String()
	pool, err := pgxpool.New(context.Background(), runtime.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	clock := newTestClock()
	r := testReplica(t, replicaSettings(t, testKeys(t)), testReplicaStores(t), pool.Ping, clock, &syncBuffer{})
	ready := func(label string, want int) {
		t.Helper()
		clock.Advance(ReadyCacheTTL) // past the last ping's answer
		answered := make(chan int, 1)
		go func() { answered <- probe(r.agent, http.MethodGet, "/ready").Code }()
		select {
		case got := <-answered:
			if got != want {
				t.Fatalf("%s: /ready answered %d, want %d", label, got, want)
			}
		case <-time.After(ReadyTimeout + 30*time.Second):
			t.Fatalf("%s: /ready didn't answer: its ping isn't bounded", label)
		}
	}
	ready("starting", http.StatusServiceUnavailable)
	r.readiness.serving()
	ready("serving", http.StatusOK)
	proxy.set("down")
	ready("the database down", http.StatusServiceUnavailable)
	ready("the database still down", http.StatusServiceUnavailable)
	proxy.set("up")
	ready("the database back", http.StatusOK)
	proxy.set("hang")
	ready("the database hanging", http.StatusServiceUnavailable)
	// Its pooled connection closed above, the ping now dials one that is
	// never answered: only ReadyTimeout ends it.
	ready("the database still hanging", http.StatusServiceUnavailable)
	proxy.mu.Lock()
	hung := proxy.hung
	proxy.mu.Unlock()
	if hung == 0 {
		t.Fatal("no ping reached the hanging database")
	}
	proxy.set("up")
	ready("the database back again", http.StatusOK)
	r.readiness.drain()
	ready("draining", http.StatusServiceUnavailable)
}
