package serve

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/telflo/opamp-server/agent"
	"github.com/telflo/opamp-server/management"
)

// /ready pings on a pool of its own, one connection, never the request
// pool. Through the whole of serve on a real database, with the request
// pool at one connection (pool_max_conns=1): a management read blocked on a
// table's lock holds that connection, and /ready still answers 200 (pinging
// the request pool it would wait its 2 s and answer 503). Serve also warns,
// once each, that OPAMP_SERVER_URL names a loopback address and a path that
// isn't the agent route.
func TestReadyPingsOnItsOwnConnectionNotTheRequestPools(t *testing.T) {
	d := testDB(t)
	runtimeURL, err := url.Parse(d.RuntimeURL)
	if err != nil {
		t.Fatal(err)
	}
	query := runtimeURL.Query()
	query.Set("pool_max_conns", "1")
	runtimeURL.RawQuery = query.Encode()
	env := envOf(map[string]string{
		DatabaseURLVariable: runtimeURL.String(), management.HostVariable: "127.0.0.1", management.PortVariable: "0",
		management.SecretVariable: testSecret, EncryptionKeyVariable: testEncryptionKey,
		agent.HostVariable: "127.0.0.1", agent.PortVariable: "0", agent.ServerURLVariable: "http://127.0.0.1:51234/collectors",
	})
	log := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exited := make(chan int, 1)
	go func() {
		exited <- serveWith(ctx, RoleAll, env, unsetNothing, slog.New(slog.NewJSONHandler(log, nil)), (&destroyRecorder{}).destroy, nil)
	}()
	agentURL := "http://" + waitForLine(t, log, "serving the agent endpoint")["address"].(string)
	management := "http://" + waitForLine(t, log, "serving the management API")["address"].(string)
	waitForLine(t, log, "the error history's cleanup ran")

	secret := "Bearer " + testSecret
	organization := uuid.NewString()
	if status, reply := over(t, http.MethodPost, management+organizationsPath, secret,
		body{"id": organization, "name": "an organization"}.bytes()); status != http.StatusCreated {
		t.Fatalf("registering: %d %s", status, reply)
	}

	// Hold the request pool's one connection: a read of the organization,
	// blocked behind a lock the test holds.
	lock, err := d.Admin.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Rollback(context.Background()) }()
	if _, err := lock.Exec(context.Background(), "lock table opamp_server.organizations in access exclusive mode"); err != nil {
		t.Fatal(err)
	}
	read := make(chan int, 1)
	go func() {
		status, _ := over(t, http.MethodGet, management+organizationsPath+"/"+organization, secret, nil)
		read <- status
	}()
	for attempt := 0; ; attempt++ {
		if attempt == 1000 {
			t.Fatal("the management read never waited on the lock")
		}
		var waiting int
		if err := d.Admin.QueryRow(context.Background(),
			`select count(*) from pg_stat_activity where datname = $1 and application_name = 'opamp-server' and wait_event_type = 'Lock'`,
			d.Name).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if status, reply := over(t, http.MethodGet, agentURL+"/ready", "", nil); status != http.StatusOK {
		t.Fatalf("/ready with the request pool's one connection held: %d %s", status, reply)
	}
	var readiness int
	if err := d.Admin.QueryRow(context.Background(),
		`select count(*) from pg_stat_activity where datname = $1 and application_name = 'opamp-server readiness'`,
		d.Name).Scan(&readiness); err != nil || readiness != 1 {
		t.Fatalf("%d readiness connections, %v; want 1", readiness, err)
	}

	if err := lock.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	if status := <-read; status != http.StatusOK {
		t.Fatalf("the management read, once the lock went: %d", status)
	}
	cancel()
	select {
	case code := <-exited:
		if code != 0 {
			t.Fatalf("serve exited %d", code)
		}
	case <-time.After(ShutdownTimeout + 30*time.Second):
		t.Fatal("serve didn't stop")
	}
	for _, warning := range []string{
		agent.ServerURLVariable + " names a loopback address",
		agent.ServerURLVariable + "'s path isn't /opamp/api/v1",
	} {
		if n := strings.Count(log.String(), warning); n != 1 {
			t.Errorf("%q logged %d times; want once", warning, n)
		}
	}
}
