package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/telflo/telflo-opamp-server/agent"
	"github.com/telflo/telflo-opamp-server/internal/testcert"
	"github.com/telflo/telflo-opamp-server/management"
	"github.com/telflo/telflo-opamp-server/migrations"
	"github.com/telflo/telflo-opamp-server/serve"
	"github.com/telflo/telflo-opamp-server/store/apikeys"
	"github.com/telflo/telflo-opamp-server/store/collectorgroups"
	"github.com/telflo/telflo-opamp-server/store/collectors"
	"github.com/telflo/telflo-opamp-server/store/database"
	"github.com/telflo/telflo-opamp-server/store/database/databasetest"
	"github.com/telflo/telflo-opamp-server/store/enrollmenttokens"
	"github.com/telflo/telflo-opamp-server/store/fleets"
	"github.com/telflo/telflo-opamp-server/store/organizations"
	"github.com/telflo/telflo-opamp-server/store/publications"
	"github.com/telflo/telflo-opamp-server/store/variables"
	"github.com/telflo/telflo-opamp-server/tokens"
)

// This package's own test database.
const testDatabase = "telflo_opamp_main_test"

// testSecret is a management secret serve accepts (32 bytes or more).
const testSecret = "main-test-secret-0123456789abcdef"

// testEncryptionKey is an enrollment token key serve accepts (32 bytes or
// more), distinctive enough to search a log for.
const testEncryptionKey = "main-test-encryption-key-marker-4c1d-0123456789"

// testServerURL is the OPAMP_SERVER_URL serve is given.
const testServerURL = "https://opamp.test.invalid/opamp/api/v1"

// serveEnv is the environment serve needs besides its database URL: both
// listeners on a free port.
func serveEnv(databaseURL string) map[string]string {
	return map[string]string{
		serve.DatabaseURLVariable:   databaseURL,
		management.PortVariable:     "0",
		management.SecretVariable:   testSecret,
		serve.EncryptionKeyVariable: testEncryptionKey,
		agent.PortVariable:          "0",
		agent.ServerURLVariable:     testServerURL,
	}
}

// testEnv is an environment serve reads with getenv and unsets with
// unsetenv, as os.Getenv and os.Unsetenv do for the process's: a test sees
// what serve unset.
type testEnv struct {
	mu     sync.Mutex
	values map[string]string
	unset  []string
}

func newTestEnv(values map[string]string) *testEnv {
	e := &testEnv{values: map[string]string{}}
	for name, value := range values {
		e.values[name] = value
	}
	return e
}

func (e *testEnv) getenv(name string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.values[name]
}

func (e *testEnv) unsetenv(name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.values, name)
	e.unset = append(e.unset, name)
	return nil
}

// isSet reports whether name is still in the environment.
func (e *testEnv) isSet(name string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, set := e.values[name]
	return set
}

// healthHandler is the management API's handler, as serve builds it, with
// no database: /health reads no store, so each store is its interface with
// nothing behind it, which panics (and answers 500) if anything calls it.
func healthHandler(t *testing.T) http.Handler {
	t.Helper()
	secrets, err := management.NewSecrets(testSecret, nil)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := tokens.NewKeyring(testEncryptionKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(keys.Destroy)
	server, err := management.New(management.Options{
		Secrets:          secrets,
		Organizations:    struct{ organizations.Store }{},
		Publications:     struct{ publications.Store }{},
		Fleets:           struct{ fleets.Store }{},
		CollectorGroups:  struct{ collectorgroups.Store }{},
		EnrollmentTokens: struct{ enrollmenttokens.Store }{},
		Collectors:       struct{ collectors.Store }{},
		Variables:        struct{ variables.Store }{},
		APIKeys:          struct{ apikeys.Store }{},
		Keys:             keys,
		Clock:            time.Now,
		Logger:           slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return server.Handler()
}

// syncBuffer is a log writer serve's goroutines and the test can share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// logLines decodes each JSON log line.
func logLines(t *testing.T, log string) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(log), "\n") {
		if line == "" {
			continue
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			t.Fatalf("log line %q isn't JSON: %v", line, err)
		}
		lines = append(lines, decoded)
	}
	return lines
}

// runCommand runs the command in-process and returns its exit code and log.
func runCommand(t *testing.T, env map[string]string, args ...string) (int, string) {
	t.Helper()
	var log syncBuffer
	e := newTestEnv(env)
	code := run(context.Background(), args, e.getenv, e.unsetenv, &log)
	return code, log.String()
}

func portOf(t *testing.T, address string) string {
	t.Helper()
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func TestHealthcheckExitsZeroAgainstARunningHealthHandler(t *testing.T) {
	server := httptest.NewServer(healthHandler(t))
	defer server.Close()

	code, log := runCommand(t, map[string]string{management.PortVariable: portOf(t, server.Listener.Addr().String())}, "healthcheck")
	if code != 0 {
		t.Fatalf("healthcheck exited %d, want 0; log: %s", code, log)
	}
}

// healthcheck agent asks the agent port's /health, read from the agent
// endpoint's settings; healthcheck management (as healthcheck alone) the
// management port's. Each fails when its own port doesn't answer, whatever
// the other's does.
func TestHealthcheckAsksTheNamedComponentsPort(t *testing.T) {
	server := httptest.NewServer(healthHandler(t))
	defer server.Close()
	answering := portOf(t, server.Listener.Addr().String())
	silent := closedListenerPort(t)
	for _, c := range []struct {
		args []string
		env  map[string]string
		want int
	}{
		{[]string{"healthcheck", "agent"}, map[string]string{agent.PortVariable: answering, management.PortVariable: silent}, 0},
		{[]string{"healthcheck", "agent"}, map[string]string{agent.PortVariable: silent, management.PortVariable: answering}, 1},
		{[]string{"healthcheck", "management"}, map[string]string{management.PortVariable: answering, agent.PortVariable: silent}, 0},
		{[]string{"healthcheck", "management"}, map[string]string{management.PortVariable: silent, agent.PortVariable: answering}, 1},
		{[]string{"healthcheck", "agent"}, map[string]string{agent.PortVariable: answering, agent.TLSCertFileVariable: "only-one"}, 1},
	} {
		if code, log := runCommand(t, c.env, c.args...); code != c.want {
			t.Errorf("%q with %v exited %d, want %d; log %s", c.args, c.env, code, c.want, log)
		}
	}
}

// closedListenerPort is a port that accepts each connection and closes it
// at once, held for the whole test.
func closedListenerPort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	return portOf(t, listener.Addr().String())
}

// Nothing answers: a listener, held for the whole test so no other process
// can take its port, that accepts each connection and closes it at once (a
// closed port could be reused by another process on macOS, and answer).
func TestHealthcheckExitsOneWhenTheConnectionIsClosedUnanswered(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	code, log := runCommand(t, map[string]string{management.PortVariable: portOf(t, listener.Addr().String())}, "healthcheck")
	if code != 1 || !strings.Contains(log, "healthcheck failed") {
		t.Fatalf("healthcheck exited %d, want 1; log: %s", code, log)
	}
}

// tlsEnv is the environment that turns TLS on, with a certificate made for
// the test.
func tlsEnv(t *testing.T) (map[string]string, testcert.Files) {
	t.Helper()
	files := testcert.New(t)
	return map[string]string{
		management.TLSCertFileVariable: files.CertFile,
		management.TLSKeyFileVariable:  files.KeyFile,
	}, files
}

// With the TLS files set, healthcheck asks over https, not verifying the
// certificate (it dials its own host); 0 on a 200, 1 on anything else, a
// plain-HTTP server included.
func TestHealthcheckAsksOverHTTPSWhenTLSIsOn(t *testing.T) {
	status := http.StatusOK
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" || r.TLS == nil {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		w.WriteHeader(status)
	}))
	server.StartTLS()
	defer server.Close()
	env, _ := tlsEnv(t)
	env[management.PortVariable] = portOf(t, server.Listener.Addr().String())

	if code, log := runCommand(t, env, "healthcheck"); code != 0 {
		t.Fatalf("healthcheck over https exited %d, want 0; log: %s", code, log)
	}
	status = http.StatusServiceUnavailable
	if code, _ := runCommand(t, env, "healthcheck"); code != 1 {
		t.Fatalf("healthcheck over https against a 503 exited %d, want 1", code)
	}

	// TLS on, but the server speaks plain HTTP: no answer counts.
	plain := httptest.NewServer(healthHandler(t))
	defer plain.Close()
	env[management.PortVariable] = portOf(t, plain.Listener.Addr().String())
	if code, _ := runCommand(t, env, "healthcheck"); code != 1 {
		t.Fatalf("healthcheck over https against plain HTTP exited %d, want 1", code)
	}
	// And TLS off against a TLS server is a failure too.
	delete(env, management.TLSCertFileVariable)
	delete(env, management.TLSKeyFileVariable)
	env[management.PortVariable] = portOf(t, server.Listener.Addr().String())
	if code, _ := runCommand(t, env, "healthcheck"); code != 1 {
		t.Fatalf("healthcheck over http against a TLS server exited %d, want 1", code)
	}
}

// healthcheck asks the address OPAMP_MANAGEMENT_HOST names when it is a
// specific one, and 127.0.0.1 when it is every interface.
func TestHealthcheckAsksTheConfiguredHost(t *testing.T) {
	server := httptest.NewServer(healthHandler(t))
	defer server.Close()
	port := portOf(t, server.Listener.Addr().String())
	for _, host := range []string{"127.0.0.1", "0.0.0.0", "::"} {
		if code, log := runCommand(t, map[string]string{management.PortVariable: port, management.HostVariable: host}, "healthcheck"); code != 0 {
			t.Fatalf("healthcheck with host %s exited %d, want 0; log: %s", host, code, log)
		}
	}

	// A server on the IPv6 loopback only: healthcheck reaches it only by
	// asking the address it was given.
	listener, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listening on ::1: %v", err)
	}
	v6 := httptest.NewUnstartedServer(healthHandler(t))
	_ = v6.Listener.Close()
	v6.Listener = listener
	v6.Start()
	defer v6.Close()
	if code, log := runCommand(t, map[string]string{management.PortVariable: portOf(t, listener.Addr().String()), management.HostVariable: "::1"}, "healthcheck"); code != 0 {
		t.Fatalf("healthcheck with host ::1 exited %d, want 0; log: %s", code, log)
	}
}

func TestHealthcheckRefusesOneTLSVariableOrABadHost(t *testing.T) {
	for _, c := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{management.TLSCertFileVariable: "/x.pem"},
			"healthcheck: OPAMP_MANAGEMENT_TLS_CERT_FILE and OPAMP_MANAGEMENT_TLS_KEY_FILE must be set together, or neither (only OPAMP_MANAGEMENT_TLS_CERT_FILE is set)"},
		{map[string]string{management.HostVariable: "localhost"},
			"healthcheck: OPAMP_MANAGEMENT_HOST must be an IP address, such as 127.0.0.1 or ::1"},
	} {
		code, log := runCommand(t, c.env, "healthcheck")
		lines := logLines(t, log)
		if code != 1 || len(lines) != 1 || lines[0]["msg"] != c.want {
			t.Errorf("healthcheck exited %d with log %s, want 1 and %q", code, log, c.want)
		}
	}
}

func TestHealthcheckExitsOneWhenHealthIsNotOK(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	code, _ := runCommand(t, map[string]string{management.PortVariable: portOf(t, server.Listener.Addr().String())}, "healthcheck")
	if code != 1 {
		t.Fatalf("healthcheck exited %d, want 1", code)
	}
}

func TestAnUnknownCommandLineExitsTwo(t *testing.T) {
	for _, args := range [][]string{
		{}, {"hello"}, {"migrate"}, {"migrate", "sideways"}, {"serve", "now"}, {"healthcheck", "now"},
		{"serve", "all"}, {"serve", "agent", "now"}, {"serve", "management", "agent"}, {"healthcheck", "agent", "now"},
		{"migrate", "up", "--yes-drop-data"}, {"migrate", "status", "now"},
		{"migrate", "down", "--yes"}, {"migrate", "down", "--yes-drop-data", "now"}, {"migrate", "--yes-drop-data", "down"},
	} {
		if code, _ := runCommand(t, nil, args...); code != 2 {
			t.Errorf("run(%q) exited %d, want 2", args, code)
		}
	}
}

func TestServeRefusesWithoutADatabaseURL(t *testing.T) {
	code, log := runCommand(t, map[string]string{serve.EncryptionKeyVariable: testEncryptionKey}, "serve")
	if code != 1 || !strings.Contains(log, "OPAMP_DATABASE_URL is not set") {
		t.Fatalf("serve exited %d with log %s, want 1 naming OPAMP_DATABASE_URL", code, log)
	}
}

func TestServeRefusesABadManagementPort(t *testing.T) {
	code, log := runCommand(t, map[string]string{management.PortVariable: "51235x", serve.EncryptionKeyVariable: testEncryptionKey,
		serve.DatabaseURLVariable: "postgresql://telflo_opamp@db.invalid/telflo_opamp"}, "serve")
	if code != 1 || !strings.Contains(log, "OPAMP_MANAGEMENT_PORT must be a port number") {
		t.Fatalf("serve exited %d with log %s, want 1 naming OPAMP_MANAGEMENT_PORT", code, log)
	}
}

func TestServeRefusesAURLThatDoesNotParseWithoutQuotingIt(t *testing.T) {
	code, log := runCommand(t, serveEnv("postgresql://someone:a-secret@localhost:notaport/telflo_opamp"), "serve")
	if code != 1 || !strings.Contains(log, "OPAMP_DATABASE_URL is not a valid PostgreSQL connection URL") {
		t.Fatalf("serve exited %d with log %s", code, log)
	}
	if strings.Contains(log, "a-secret") || strings.Contains(log, "someone") {
		t.Fatalf("the log quotes the URL: %s", log)
	}
}

// refusalHangGuard bounds how long assertRefused waits for serve to refuse:
// a generous hang guard, never a timing. A serve that starts instead of
// refusing would otherwise run until go test's own timeout.
const refusalHangGuard = 60 * time.Second

// assertRefused checks serve's refusal: exit 1, exactly one log line, at
// error level, whose message is want, and no URL or password anywhere.
func assertRefused(t *testing.T, databaseURL, want string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var output syncBuffer
	exited := make(chan int, 1)
	env := newTestEnv(serveEnv(databaseURL))
	go func() { exited <- run(ctx, []string{"serve"}, env.getenv, env.unsetenv, &output) }()
	var code int
	select {
	case code = <-exited:
	case <-time.After(refusalHangGuard):
		cancel()
		select {
		case <-exited:
		case <-time.After(serve.ShutdownTimeout + 5*time.Second):
		}
		t.Fatalf("serve didn't refuse within %s (it started instead); log: %s", refusalHangGuard, output.String())
	}
	log := output.String()
	if code != 1 {
		t.Fatalf("serve exited %d, want 1; log: %s", code, log)
	}
	lines := logLines(t, log)
	if len(lines) != 1 {
		t.Fatalf("serve logged %d lines, want 1: %s", len(lines), log)
	}
	if lines[0]["level"] != "ERROR" || lines[0]["msg"] != want {
		t.Fatalf("serve logged %v, want an ERROR %q", lines[0], want)
	}
	password := ""
	if i := strings.Index(databaseURL, "@"); i > 0 {
		password = databaseURL[strings.LastIndex(databaseURL[:i], ":")+1 : i]
	}
	if strings.Contains(log, "postgresql://") || (password != "" && strings.Contains(log, password)) {
		t.Fatalf("the refusal quotes the URL or its password: %s", log)
	}
}

const roleHint = " Set OPAMP_DATABASE_URL to the server's own login role (telflo_opamp, made by " +
	"deploy/bootstrap.sql), a member of telflo_opamp_runtime, and OPAMP_MIGRATION_DATABASE_URL to telflo_opamp_owner."

func TestServeRefusesTheOwner(t *testing.T) {
	d := databasetest.New(t, testDatabase)
	assertRefused(t, d.OwnerURL,
		`Refusing to start: the database role "telflo_opamp_owner" is telflo_opamp_owner or a member of it, `+
			`owns objects in the telflo_opamp schema, or is a member of their owner, `+
			`doesn't inherit telflo_opamp_runtime's privileges.`+roleHint)
}

func TestServeRefusesASuperuser(t *testing.T) {
	d := databasetest.New(t, testDatabase)
	name, roleURL := d.ThrowawayRole(t, "superuser")
	assertRefused(t, roleURL, `Refusing to start: the database role "`+name+`" is a superuser.`+roleHint)
}

func TestServeRefusesARoleWithBypassRLS(t *testing.T) {
	d := databasetest.New(t, testDatabase)
	// Otherwise a role serve would accept: not a superuser, owning nothing,
	// inheriting the runtime role. Only BYPASSRLS can refuse it.
	name, roleURL := d.ThrowawayRole(t, "nosuperuser bypassrls")
	if _, err := d.Admin.Exec(context.Background(), "grant telflo_opamp_runtime to "+name+" with inherit true"); err != nil {
		t.Fatal(err)
	}
	assertRefused(t, roleURL, `Refusing to start: the database role "`+name+`" has BYPASSRLS.`+roleHint)
}

// serveShapedRole is a throwaway role serve would accept, as bootstrap.sql
// shapes telflo_opamp (a member of telflo_opamp_runtime with inheritance,
// its errors terse), plus attributes. The shared telflo_opamp is never
// altered: other packages' tests use it at the same time.
func serveShapedRole(t *testing.T, d *databasetest.Databases, attributes string) (name, roleURL string) {
	t.Helper()
	name, roleURL = d.ThrowawayRole(t, "nosuperuser nobypassrls "+attributes)
	for _, statement := range []string{
		"grant telflo_opamp_runtime to " + name + " with inherit true",
		"alter role " + name + " set log_error_verbosity = 'terse'",
	} {
		if _, err := d.Admin.Exec(context.Background(), statement); err != nil {
			t.Fatal(err)
		}
	}
	return name, roleURL
}

// serve refuses what the bootstrap leaves out and its role checks don't
// see, one fixed line each: CREATEDB, CREATEROLE, PUBLIC's CONNECT and
// TEMPORARY on the database (granted on this package's own database, which
// alters no shared role), and errors that aren't terse.
func TestServeRefusesWhatTheBootstrapLeavesOut(t *testing.T) {
	cases := []struct {
		name string
		url  func(t *testing.T, d *databasetest.Databases) string
		want string
	}{
		{"CREATEDB", func(t *testing.T, d *databasetest.Databases) string {
			_, roleURL := serveShapedRole(t, d, "createdb")
			return roleURL
		}, database.CreateDBRefusal},
		{"CREATEROLE", func(t *testing.T, d *databasetest.Databases) string {
			_, roleURL := serveShapedRole(t, d, "createrole")
			return roleURL
		}, database.CreateRoleRefusal},
		{"PUBLIC's CONNECT", func(t *testing.T, d *databasetest.Databases) string {
			if _, err := d.Admin.Exec(context.Background(), "grant connect on database "+testDatabase+" to public"); err != nil {
				t.Fatal(err)
			}
			return d.RuntimeURL
		}, database.PublicConnectRefusal},
		{"PUBLIC's TEMPORARY", func(t *testing.T, d *databasetest.Databases) string {
			if _, err := d.Admin.Exec(context.Background(), "grant temporary on database "+testDatabase+" to public"); err != nil {
				t.Fatal(err)
			}
			return d.RuntimeURL
		}, database.PublicTemporaryRefusal},
		{"verbosity", func(t *testing.T, d *databasetest.Databases) string {
			name, roleURL := serveShapedRole(t, d, "")
			if _, err := d.Admin.Exec(context.Background(), "alter role "+name+" set log_error_verbosity = 'default'"); err != nil {
				t.Fatal(err)
			}
			return roleURL
		}, database.VerbosityRefusal},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := databasetest.New(t, testDatabase)
			assertRefused(t, c.url(t, d), c.want)
		})
	}
}

// canaryText is a distinctive random string to search output for.
func canaryText(t *testing.T) string {
	t.Helper()
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "canary" + hex.EncodeToString(b)
}

// No line serve or migrate up logs quotes a part of a database URL's
// password, however the driver takes the URL apart: a password holding `@`
// once put its tail in a "can't connect" line, since pgx ends the user part
// at the first `@` and fails to resolve the rest as the host. Each password
// is two canaries around the character; the output is searched for each by
// boolean and never printed (a failure names the path, not what leaked).
// What each path reports shows it reached the database driver.
func TestDatabaseErrorsQuoteNoPartOfThePassword(t *testing.T) {
	d := databasetest.New(t, testDatabase)
	u, err := url.Parse(d.RuntimeURL)
	if err != nil {
		t.Fatal("the runtime URL doesn't parse")
	}
	const (
		unreachable    = "the database's host can't be reached or its name resolved"
		authentication = "authentication failed (SQLSTATE 28P01)"
	)
	for _, c := range []struct{ separator, want string }{
		{"@", unreachable},
		{"?", authentication},
		{"#", authentication},
		{"%40", authentication},
	} {
		before, after := canaryText(t), canaryText(t)
		databaseURL := "postgresql://" + database.ServerRole + ":" + before + c.separator + after + "@" + u.Host + "/" + testDatabase
		for _, path := range []struct {
			name string
			env  map[string]string
			args []string
		}{
			{"serve", serveEnv(databaseURL), []string{"serve"}},
			{"migrate up", map[string]string{migrationDatabaseURLVariable: databaseURL}, []string{"migrate", "up"}},
		} {
			code, log := runCommand(t, path.env, path.args...)
			if strings.Contains(log, before) || strings.Contains(log, after) {
				t.Errorf("%s logged a part of a password holding %q", path.name, c.separator)
				continue
			}
			if code != 1 || !strings.Contains(log, c.want) {
				t.Errorf("%s with a password holding %q exited %d, want 1 and a line saying %q", path.name, c.separator, code, c.want)
			}
		}
	}
}

// latestMigration is the version every test that names a migration number
// derives it from, so a new migration doesn't break them.
func latestMigration(t *testing.T) int64 {
	t.Helper()
	latest, err := database.LatestMigration()
	if err != nil {
		t.Fatal(err)
	}
	return latest
}

func TestServeRefusesASchemaOneMigrationBehind(t *testing.T) {
	d := databasetest.New(t, testDatabase)
	latest := latestMigration(t)
	code, log := runCommand(t, map[string]string{migrationDatabaseURLVariable: d.OwnerURL}, "migrate", "down", "--yes-drop-data")
	if code != 0 {
		t.Fatalf("migrate down exited %d: %s", code, log)
	}
	assertRefused(t, d.RuntimeURL, fmt.Sprintf(
		"Refusing to start: the schema is at %d, this build needs %d: run `opamp-server migrate up`", latest-1, latest))
}

func TestServeRefusesASchemaAheadOfThisBuild(t *testing.T) {
	d := databasetest.New(t, testDatabase)
	latest := latestMigration(t)
	// A migration from a newer release, as goose records one.
	if _, err := d.Owner.Exec(context.Background(),
		"insert into telflo_opamp.goose_db_version (version_id, is_applied) values ($1, true)", latest+1); err != nil {
		t.Fatal(err)
	}
	assertRefused(t, d.RuntimeURL, fmt.Sprintf(
		"Refusing to start: the schema is at %d, which is newer than this build (it needs %d): "+
			"run the opamp-server release that migrated it", latest+1, latest))
}

func TestServeStartsAsTheServerRoleAndAnswersHealth(t *testing.T) {
	d := databasetest.New(t, testDatabase)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var log syncBuffer
	exited := make(chan int, 1)
	go func() {
		env := newTestEnv(serveEnv(d.RuntimeURL))
		exited <- run(ctx, []string{"serve"}, env.getenv, env.unsetenv, &log)
	}()

	// The address serve logs, not 127.0.0.1: on macOS serve's wildcard
	// listener can be given a port another process holds on 127.0.0.1 (an
	// editor's helper did, answering 404), and that process gets the IPv4
	// loopback's connections. Its wildcard address reaches serve alone. For
	// the same reason `healthcheck`, which asks 127.0.0.1, is tested against
	// its own 127.0.0.1 server above, and against serve by the compose
	// stack's health check.
	address := waitForListening(t, &log, exited)
	if status, body := request(t, http.MethodGet, "http://"+address+"/health", "", ""); status != http.StatusOK || body != "ok" {
		t.Fatalf("GET /health = %d %q, want 200 \"ok\"", status, body)
	}

	cancel()
	select {
	case code := <-exited:
		if code != 0 {
			t.Fatalf("serve exited %d after its context ended, want 0; log: %s", code, log.String())
		}
	case <-time.After(serve.ShutdownTimeout + 5*time.Second):
		t.Fatal("serve didn't stop after its context ended")
	}
	if strings.Contains(log.String(), databasetest.RuntimePassword) {
		t.Fatalf("serve logged the password: %s", log.String())
	}
}

// waitForListening reads serve's log until it names the address it listens
// on, failing if serve exits first.
func waitForListening(t *testing.T, log *syncBuffer, exited <-chan int) string {
	t.Helper()
	// Bounded by attempts, not a fixed wait: each one sleeps briefly.
	for attempt := 0; attempt < 500; attempt++ {
		select {
		case code := <-exited:
			t.Fatalf("serve exited %d before listening; log: %s", code, log.String())
		default:
		}
		for _, line := range logLines(t, log.String()) {
			if line["msg"] == "serving the management API" {
				return line["address"].(string)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("serve never logged its address; log: %s", log.String())
	return ""
}

func TestMigrateUpStatusAndDownThroughTheCommandLine(t *testing.T) {
	d := databasetest.New(t, testDatabase, databasetest.Unmigrated())
	env := map[string]string{migrationDatabaseURLVariable: d.OwnerURL}

	schemaVersion := func() (current, latest float64) {
		t.Helper()
		code, log := runCommand(t, env, "migrate", "status")
		if code != 0 {
			t.Fatalf("migrate status exited %d: %s", code, log)
		}
		for _, line := range logLines(t, log) {
			if line["msg"] == "schema version" {
				return line["current"].(float64), line["latest"].(float64)
			}
		}
		t.Fatalf("migrate status reported no version: %s", log)
		return 0, 0
	}

	latest := float64(latestMigration(t))
	files, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	code, log := runCommand(t, env, "migrate", "up")
	if code != 0 {
		t.Fatalf("migrate up exited %d: %s", code, log)
	}
	for _, file := range files {
		if !strings.Contains(log, `"file":"`+file+`"`) {
			t.Fatalf("migrate up didn't name %s: %s", file, log)
		}
	}
	if current, latestReported := schemaVersion(); current != latest || latestReported != latest {
		t.Fatalf("after up, status reports %v of %v, want %v of %v", current, latestReported, latest, latest)
	}
	if code, log := runCommand(t, env, "migrate", "up"); code != 0 || !strings.Contains(log, "no migration to apply") {
		t.Fatalf("a second migrate up exited %d: %s", code, log)
	}
	// Each down rolls back one migration.
	for want := latest - 1; want >= 0; want-- {
		if code, log := runCommand(t, env, "migrate", "down", "--yes-drop-data"); code != 0 {
			t.Fatalf("migrate down exited %d: %s", code, log)
		}
		if current, _ := schemaVersion(); current != want {
			t.Fatalf("after down, status reports %v, want %v", current, want)
		}
	}
	if code, log := runCommand(t, env, "migrate", "down", "--yes-drop-data"); code != 0 || !strings.Contains(log, "no migration to roll back") {
		t.Fatalf("migrate down at 0 exited %d: %s", code, log)
	}
	if code, log := runCommand(t, env, "migrate", "up"); code != 0 {
		t.Fatalf("migrate up again exited %d: %s", code, log)
	}
	if current, _ := schemaVersion(); current != latest {
		t.Fatalf("after up again, status reports %v, want %v", current, latest)
	}
}

// `migrate` as the server's role is refused with a message naming the
// setting and the owner role, not the bare permission error (42501) the
// first `create schema` would end on.
func TestMigrateAsTheServerRoleIsRefusedNamingTheOwner(t *testing.T) {
	d := databasetest.New(t, testDatabase, databasetest.Unmigrated())
	code, log := runCommand(t, map[string]string{migrationDatabaseURLVariable: d.RuntimeURL}, "migrate", "up")
	if code != 1 {
		t.Fatalf("migrate up as telflo_opamp exited %d, want 1: %s", code, log)
	}
	for _, want := range []string{
		`refusing to migrate: the role \"telflo_opamp\" doesn't own the database`,
		"OPAMP_MIGRATION_DATABASE_URL", "telflo_opamp_owner",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("the log lacks %q: %s", want, log)
		}
	}
	for _, leaked := range []string{"42501", "SQLSTATE", "permission denied", databasetest.RuntimePassword} {
		if strings.Contains(log, leaked) {
			t.Errorf("the log holds %q: %s", leaked, log)
		}
	}
}

// `migrate down` drops a migration's tables and their rows, so it runs
// only with --yes-drop-data, and says why and what to do instead. It
// refuses before it connects: here there is no URL at all.
func TestMigrateDownRefusesWithoutYesDropData(t *testing.T) {
	code, log := runCommand(t, map[string]string{}, "migrate", "down")
	lines := logLines(t, log)
	want := "Refusing to migrate down: it rolls back the newest migration, dropping its tables and every row in them. " +
		"down is for development; to roll back a release, restore the backup taken before `migrate up`. " +
		"To roll back anyway, run `opamp-server migrate down --yes-drop-data`."
	if code != 1 || len(lines) != 1 || lines[0]["msg"] != want {
		t.Fatalf("migrate down exited %d with log %s, want 1 and %q", code, log, want)
	}
}

// And with a database, the refusal changes nothing: the schema stays.
func TestMigrateDownWithoutYesDropDataLeavesTheSchema(t *testing.T) {
	d := databasetest.New(t, testDatabase)
	if code, log := runCommand(t, map[string]string{migrationDatabaseURLVariable: d.OwnerURL}, "migrate", "down"); code != 1 {
		t.Fatalf("migrate down exited %d, want 1: %s", code, log)
	}
	var exists bool
	if err := d.Owner.QueryRow(context.Background(),
		"select to_regclass('telflo_opamp.publications') is not null").Scan(&exists); err != nil || !exists {
		t.Fatalf("after a refused down, the table exists = %v, %v; want true", exists, err)
	}
}

func TestMigrateRefusesWithoutItsURL(t *testing.T) {
	code, log := runCommand(t, map[string]string{serve.DatabaseURLVariable: "postgresql://x@y/z"}, "migrate", "up")
	if code != 1 || !strings.Contains(log, "OPAMP_MIGRATION_DATABASE_URL is not set") {
		t.Fatalf("migrate up exited %d with log %s", code, log)
	}
}

// unreachableURL parses but names a closed port, so a serve that gets past
// its settings fails at the database instead.
const unreachableURL = "postgresql://nobody:not-a-real-password@127.0.0.1:1/telflo_opamp"

func TestServeRefusesAMissingOrShortSecret(t *testing.T) {
	short := strings.Repeat("s", management.MinSecretBytes-1)
	shortPrevious := strings.Repeat("p", management.MinSecretBytes-1)
	cases := []struct {
		name, secret, previous, want string
	}{
		{"missing", "", "", "Refusing to start: OPAMP_MANAGEMENT_SECRET is not set"},
		{"only whitespace", "  \n", "", "Refusing to start: OPAMP_MANAGEMENT_SECRET is not set"},
		{"31 bytes", short, "", "Refusing to start: OPAMP_MANAGEMENT_SECRET must be at least 32 bytes"},
		{"a short previous one", testSecret, testSecret + "-old," + shortPrevious,
			"Refusing to start: OPAMP_MANAGEMENT_SECRETS_PREVIOUS: entry 2 of 2 must be at least 32 bytes"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := serveEnv(unreachableURL)
			env[management.SecretVariable] = c.secret
			env[management.PreviousSecretsVariable] = c.previous
			code, log := runCommand(t, env, "serve")
			lines := logLines(t, log)
			if code != 1 || len(lines) != 1 || lines[0]["msg"] != c.want {
				t.Fatalf("serve exited %d with log %s, want 1 and one line %q", code, log, c.want)
			}
			for _, secret := range []string{short, shortPrevious, testSecret} {
				if strings.Contains(log, secret) {
					t.Fatalf("the refusal quotes a secret: %s", log)
				}
			}
		})
	}
}

// The secrets' checks pass at 32 bytes, and empty entries in the previous
// list (a trailing comma, ",,") are ignored rather than refused as short:
// serve gets as far as the database.
func TestServeAcceptsA32ByteSecretAndIgnoresEmptyPreviousEntries(t *testing.T) {
	env := serveEnv(unreachableURL)
	env[management.SecretVariable] = strings.Repeat("c", management.MinSecretBytes)
	env[management.PreviousSecretsVariable] = ",," + strings.Repeat("p", management.MinSecretBytes) + ", ,"
	code, log := runCommand(t, env, "serve")
	if code != 1 || strings.Contains(log, "SECRET") || !strings.Contains(log, "can't check the database role") {
		t.Fatalf("serve exited %d with log %s, want a refusal at the database, not the secrets", code, log)
	}
}

// startServe runs serve with env until the test ends, and returns the
// address it listens on and its log.
func startServe(t *testing.T, env *testEnv) (string, *syncBuffer) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	log := &syncBuffer{}
	exited := make(chan int, 1)
	go func() { exited <- run(ctx, []string{"serve"}, env.getenv, env.unsetenv, log) }()
	address := waitForListening(t, log, exited)
	t.Cleanup(func() {
		cancel()
		select {
		case code := <-exited:
			if code != 0 {
				t.Errorf("serve exited %d after its context ended, want 0; log: %s", code, log.String())
			}
		case <-time.After(serve.ShutdownTimeout + 5*time.Second):
			t.Error("serve didn't stop after its context ended")
		}
	})
	return address, log
}

// request sends one request to serve and returns its status and body.
func request(t *testing.T, method, url, authorization, body string) (int, string) {
	t.Helper()
	return requestWithType(t, method, url, authorization, "", body)
}

func requestWithType(t *testing.T, method, url, authorization, contentType, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	reply, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(reply)
}

// serve reads the enrollment tokens' keys and then unsets both value
// variables, OPAMP_ENCRYPTION_KEY and OPAMP_ENCRYPTION_KEYS_PREVIOUS, from
// its environment before it listens: main passes os.Unsetenv. Nothing reads
// them from the process's own environment instead.
func TestServeReadsTheEncryptionKeysThenUnsetsThem(t *testing.T) {
	d := databasetest.New(t, testDatabase)
	// Out of the process's environment too, for anything that reads
	// os.Getenv.
	for _, pair := range os.Environ() {
		name, _, _ := strings.Cut(pair, "=")
		if strings.HasPrefix(name, "OPAMP_ENCRYPTION_KEY") {
			t.Setenv(name, "")
			if err := os.Unsetenv(name); err != nil {
				t.Fatal(err)
			}
		}
	}

	values := serveEnv(d.RuntimeURL)
	values[serve.PreviousEncryptionKeysVariable] = "an-older-encryption-key-marker-7a2e-0123456789"
	env := newTestEnv(values)
	address, log := startServe(t, env)
	if status, _ := request(t, http.MethodGet, "http://"+address+"/api/v1/nothing-here", "Bearer "+testSecret, ""); status != http.StatusNotFound {
		t.Fatalf("an authenticated request answered %d, want 404", status)
	}
	for _, name := range []string{serve.EncryptionKeyVariable, serve.PreviousEncryptionKeysVariable} {
		if env.isSet(name) {
			t.Fatalf("serve left %s in its environment", name)
		}
	}
	env.mu.Lock()
	unset := append([]string(nil), env.unset...)
	env.mu.Unlock()
	if !slices.Contains(unset, serve.EncryptionKeyVariable) || !slices.Contains(unset, serve.PreviousEncryptionKeysVariable) {
		t.Fatalf("serve unset %v, want both key variables", unset)
	}
	if !strings.Contains(log.String(), `"encryptionKeys":2`) {
		t.Fatalf("serve's startup line doesn't count the current and the previous key: %s", log.String())
	}
	for _, key := range []string{testEncryptionKey, "marker-4c1d", "marker-7a2e"} {
		if strings.Contains(log.String(), key) {
			t.Fatalf("serve's log holds a key: %s", log.String())
		}
	}
}

// main passes os.Unsetenv: serve run as the process would be leaves neither
// key variable in the process's environment, even when it refuses to start.
func TestServeUnsetsTheKeyVariablesFromTheProcessEnvironment(t *testing.T) {
	t.Setenv(serve.EncryptionKeyVariable, testEncryptionKey)
	t.Setenv(serve.PreviousEncryptionKeysVariable, "an-older-encryption-key-marker-7a2e-0123456789")
	t.Setenv(serve.DatabaseURLVariable, "")
	var log syncBuffer
	code := run(context.Background(), []string{"serve"}, os.Getenv, os.Unsetenv, &log)
	if code != 1 || !strings.Contains(log.String(), "OPAMP_DATABASE_URL is not set") {
		t.Fatalf("serve exited %d with log %s, want a refusal at the database URL", code, log.String())
	}
	for _, name := range []string{serve.EncryptionKeyVariable, serve.PreviousEncryptionKeysVariable} {
		if _, set := os.LookupEnv(name); set {
			t.Fatalf("%s is still in the process's environment", name)
		}
	}
}

// The keys from files, a compose secret's way; each file's final line
// ending isn't part of the key.
func TestServeReadsTheEncryptionKeysFromFiles(t *testing.T) {
	d := databasetest.New(t, testDatabase)
	dir := t.TempDir()
	current, previous := dir+"/key", dir+"/previous"
	if err := os.WriteFile(current, []byte(testEncryptionKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(previous, []byte("an-older-encryption-key-marker-7a2e-0123456789, another-older-key-marker-0123456789abc\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	values := serveEnv(d.RuntimeURL)
	delete(values, serve.EncryptionKeyVariable)
	values[serve.EncryptionKeyFileVariable] = current
	values[serve.PreviousEncryptionKeysFileVariable] = previous
	_, log := startServe(t, newTestEnv(values))
	if !strings.Contains(log.String(), `"encryptionKeys":3`) {
		t.Fatalf("serve's startup line doesn't count three keys: %s", log.String())
	}
}

// serve refuses key settings it can't use, before it touches the database,
// naming the variable and never a key.
func TestServeRefusesEncryptionKeySettingsItCannotUse(t *testing.T) {
	dir := t.TempDir()
	keyFile := dir + "/key"
	if err := os.WriteFile(keyFile, []byte(testEncryptionKey), 0o600); err != nil {
		t.Fatal(err)
	}
	shortFile := dir + "/short"
	if err := os.WriteFile(shortFile, []byte(strings.Repeat("k", 31)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	short := "short-key-marker-91f0"
	older := func(n int) string {
		keys := make([]string, n)
		for i := range keys {
			keys[i] = fmt.Sprintf("an-older-key-marker-%02d-0123456789abcdef", i)
		}
		return strings.Join(keys, ",")
	}
	for _, c := range []struct {
		name string
		keys map[string]string
		want string
	}{
		{"no key", map[string]string{serve.EncryptionKeyVariable: ""},
			"Refusing to start: OPAMP_ENCRYPTION_KEY or OPAMP_ENCRYPTION_KEY_FILE must be set"},
		{"both the value and the file", map[string]string{serve.EncryptionKeyFileVariable: keyFile},
			"Refusing to start: OPAMP_ENCRYPTION_KEY and OPAMP_ENCRYPTION_KEY_FILE are both set; set one"},
		{"a key of 31 bytes", map[string]string{serve.EncryptionKeyVariable: short + strings.Repeat("k", 31-len(short))},
			"Refusing to start: OPAMP_ENCRYPTION_KEY must be UTF-8 of at least 32 bytes"},
		{"a key file of 31 bytes", map[string]string{serve.EncryptionKeyVariable: "", serve.EncryptionKeyFileVariable: shortFile},
			"Refusing to start: OPAMP_ENCRYPTION_KEY_FILE must be UTF-8 of at least 32 bytes"},
		{"a key file that doesn't exist", map[string]string{serve.EncryptionKeyVariable: "", serve.EncryptionKeyFileVariable: keyFile + ".missing"},
			"Refusing to start: OPAMP_ENCRYPTION_KEY_FILE names a file that doesn't exist"},
		{"a previous key of 31 bytes", map[string]string{serve.PreviousEncryptionKeysVariable: older(1) + "," + short + strings.Repeat("k", 31-len(short))},
			"Refusing to start: OPAMP_ENCRYPTION_KEYS_PREVIOUS: entry 2 of 2 must be UTF-8 of at least 32 bytes"},
		{"nine previous keys", map[string]string{serve.PreviousEncryptionKeysVariable: older(9)},
			"Refusing to start: OPAMP_ENCRYPTION_KEYS_PREVIOUS holds 9 keys; at most 8 are accepted"},
		{"both previous settings", map[string]string{serve.PreviousEncryptionKeysVariable: older(1),
			serve.PreviousEncryptionKeysFileVariable: keyFile},
			"Refusing to start: OPAMP_ENCRYPTION_KEYS_PREVIOUS and OPAMP_ENCRYPTION_KEYS_PREVIOUS_FILE are both set; set one"},
	} {
		t.Run(c.name, func(t *testing.T) {
			values := serveEnv(unreachableURL)
			for name, value := range c.keys {
				values[name] = value
			}
			env := newTestEnv(values)
			var output syncBuffer
			code := run(context.Background(), []string{"serve"}, env.getenv, env.unsetenv, &output)
			log := output.String()
			lines := logLines(t, log)
			if code != 1 || len(lines) != 1 || lines[0]["msg"] != c.want {
				t.Fatalf("serve exited %d with log %s, want 1 and one line %q", code, log, c.want)
			}
			for _, leaked := range []string{testEncryptionKey, short, "marker-0", "kkkkkkkk"} {
				if strings.Contains(log, leaked) {
					t.Fatalf("the refusal quotes a key: %s", log)
				}
			}
			if env.isSet(serve.EncryptionKeyVariable) || env.isSet(serve.PreviousEncryptionKeysVariable) {
				t.Fatal("a refused serve left a key variable in its environment")
			}
		})
	}
	// Eight previous keys, empty entries and spaces ignored, pass: serve
	// gets as far as the database.
	values := serveEnv(unreachableURL)
	values[serve.PreviousEncryptionKeysVariable] = ", " + strings.ReplaceAll(older(8), ",", " , ") + ",,"
	code, log := runCommand(t, values, "serve")
	if code != 1 || strings.Contains(log, "ENCRYPTION") || !strings.Contains(log, "can't check the database role") {
		t.Fatalf("serve exited %d with log %s, want a refusal at the database, not the keys", code, log)
	}
}

// serve's log holds neither the secret, right or wrong, nor a request body.
func TestServeLogsNeitherTheSecretNorABody(t *testing.T) {
	d := databasetest.New(t, testDatabase)
	address, log := startServe(t, newTestEnv(serveEnv(d.RuntimeURL)))
	const marker = "receivers:\n  otlp: # log-marker-7f3a"
	const wrong = "a-wrong-secret-that-is-long-enough-0123"

	status, body := request(t, http.MethodPut, "http://"+address+"/api/v1/organizations/x/unknown/y", "Bearer "+testSecret, marker)
	if status != http.StatusNotFound || !strings.Contains(body, `"not_found"`) {
		t.Fatalf("with the secret: %d %s, want 404 not_found", status, body)
	}
	status, body = request(t, http.MethodPut, "http://"+address+"/api/v1/organizations/x/unknown/y", "Bearer "+wrong, marker)
	if status != http.StatusUnauthorized || !strings.Contains(body, `"unauthorized"`) {
		t.Fatalf("with a wrong secret: %d %s, want 401 unauthorized", status, body)
	}
	// A real organization and a publication holding the markers, stored
	// through serve's own stores on the database.
	organization := uuid.NewString()
	status, body = requestWithType(t, http.MethodPost, "http://"+address+"/api/v1/organizations", "Bearer "+testSecret,
		"application/json", `{"id":"`+organization+`","name":"org-name-marker-2b9's workspace"}`)
	if status != http.StatusCreated {
		t.Fatalf("POST an organization: %d %s, want 201", status, body)
	}
	publications := "http://" + address + "/api/v1/organizations/" + organization + "/publications"
	status, body = requestWithType(t, http.MethodPost, publications, "Bearer "+testSecret, "application/json", publicationJSON(marker))
	var created struct{ ID string }
	if status != http.StatusCreated || json.Unmarshal([]byte(body), &created) != nil {
		t.Fatalf("POST a publication: %d %s, want 201", status, body)
	}
	target := publications + "/" + created.ID
	status, body = request(t, http.MethodGet, target, "Bearer "+testSecret, "")
	var stored struct{ YAML string }
	if status != http.StatusOK || json.Unmarshal([]byte(body), &stored) != nil || stored.YAML != marker {
		t.Fatalf("GET the publication: %d %s, want 200 with the YAML", status, body)
	}

	// A fleet, whose label and bearers are secrets of the log's too.
	status, body = requestWithType(t, http.MethodPost, "http://"+address+"/api/v1/organizations/"+organization+"/fleets",
		"Bearer "+testSecret, "application/json",
		`{"label":"fleet-label-marker-0d3e","type":"server","rotateExpiresAt":null,"actor":{"id":"`+uuid.NewString()+`","email":"ana@acme.io"}}`)
	var fleet struct {
		Tokens struct {
			Default struct{ Bearer string } `json:"default"`
			Rotate  struct{ Bearer string } `json:"rotate"`
		} `json:"tokens"`
	}
	if status != http.StatusCreated || json.Unmarshal([]byte(body), &fleet) != nil || fleet.Tokens.Rotate.Bearer == "" {
		t.Fatalf("POST a fleet: %d %s, want 201 with its bearers", status, body)
	}

	logged := log.String()
	for _, leaked := range []string{"fleet-label-marker-0d3e", fleet.Tokens.Default.Bearer, fleet.Tokens.Rotate.Bearer, testEncryptionKey} {
		if strings.Contains(logged, leaked) {
			t.Fatalf("serve's log holds a fleet's label, a bearer or the key: %s", logged)
		}
	}
	for _, want := range []string{`"status":404`, `"status":401`, `"status":201`, `"status":200`} {
		if !strings.Contains(logged, want) {
			t.Fatalf("serve didn't log every request (%s): %s", want, logged)
		}
	}
	for _, leaked := range []string{testSecret, wrong, "log-marker-7f3a", "Bearer", "organizations/x", "ana@acme.io",
		"org-name-marker-2b9", organization} {
		if strings.Contains(logged, leaked) {
			t.Fatalf("serve's log holds %q: %s", leaked, logged)
		}
	}
}

// publicationJSON is a valid PUT body for yaml.
func publicationJSON(yaml string) string {
	digest := sha256.Sum256([]byte(yaml))
	body, err := json.Marshal(map[string]any{
		"configuration": map[string]any{"id": uuid.NewString(), "name": "payments-gateway"},
		"version":       map[string]any{"id": uuid.NewString(), "number": 1, "label": nil},
		"yaml":          yaml,
		"sha256":        hex.EncodeToString(digest[:]),
		"publishedBy":   map[string]any{"id": uuid.NewString(), "email": "ana@acme.io"},
		"publishedAt":   "2026-10-03T14:02:11.000Z",
		"collector":     map[string]any{"distribution": "otelcol-contrib", "version": "0.162.0"},
	})
	if err != nil {
		panic(err)
	}
	return string(body)
}

// serve refuses one TLS variable without the other, and a file it can't
// use, before it touches the database, naming the variable and never the
// file's content.
func TestServeRefusesTLSSettingsItCannotUse(t *testing.T) {
	files := testcert.New(t)
	garbage := t.TempDir() + "/garbage.pem"
	if err := os.WriteFile(garbage, []byte("not-a-certificate-marker-5e0"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		tls  map[string]string
		want string
	}{
		{"only the certificate", map[string]string{management.TLSCertFileVariable: files.CertFile},
			"Refusing to start: OPAMP_MANAGEMENT_TLS_CERT_FILE and OPAMP_MANAGEMENT_TLS_KEY_FILE must be set together, or neither (only OPAMP_MANAGEMENT_TLS_CERT_FILE is set)"},
		{"only the key", map[string]string{management.TLSKeyFileVariable: files.KeyFile},
			"Refusing to start: OPAMP_MANAGEMENT_TLS_CERT_FILE and OPAMP_MANAGEMENT_TLS_KEY_FILE must be set together, or neither (only OPAMP_MANAGEMENT_TLS_KEY_FILE is set)"},
		{"a certificate path that doesn't exist", map[string]string{
			management.TLSCertFileVariable: files.CertFile + ".missing", management.TLSKeyFileVariable: files.KeyFile},
			"Refusing to start: OPAMP_MANAGEMENT_TLS_CERT_FILE names a file that doesn't exist"},
		{"a certificate that isn't one", map[string]string{
			management.TLSCertFileVariable: garbage, management.TLSKeyFileVariable: files.KeyFile},
			"Refusing to start: OPAMP_MANAGEMENT_TLS_CERT_FILE and OPAMP_MANAGEMENT_TLS_KEY_FILE aren't a PEM certificate and its matching private key"},
		{"a host that isn't an IP address", map[string]string{management.HostVariable: "opamp.internal"},
			"Refusing to start: OPAMP_MANAGEMENT_HOST must be an IP address, such as 127.0.0.1 or ::1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := serveEnv(unreachableURL)
			for name, value := range c.tls {
				env[name] = value
			}
			code, log := runCommand(t, env, "serve")
			lines := logLines(t, log)
			if code != 1 || len(lines) != 1 || lines[0]["msg"] != c.want {
				t.Fatalf("serve exited %d with log %s, want 1 and one line %q", code, log, c.want)
			}
			if strings.Contains(log, "marker-5e0") || strings.Contains(log, "PRIVATE KEY") {
				t.Fatalf("the refusal quotes a file: %s", log)
			}
		})
	}
}

// serve with TLS on, bound to 127.0.0.1, as telflo_opamp on a real
// database: a client trusting the certificate gets /health and the API
// (401 without the secret), and `healthcheck`, with the same settings, asks
// it over https and exits 0.
func TestServeOverTLSOnTheConfiguredHostAndItsHealthcheck(t *testing.T) {
	d := databasetest.New(t, testDatabase)
	env, files := tlsEnv(t)
	for name, value := range serveEnv(d.RuntimeURL) {
		env[name] = value
	}
	env[management.HostVariable] = "127.0.0.1"
	address, log := startServe(t, newTestEnv(env))
	if !strings.HasPrefix(address, "127.0.0.1:") || !strings.Contains(log.String(), `"tls":true`) {
		t.Fatalf("serve listens on %s, want 127.0.0.1 with TLS; log: %s", address, log.String())
	}

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: files.Pool}, Proxy: nil}}
	for _, c := range []struct {
		path, authorization string
		status              int
	}{
		{"/health", "", http.StatusOK},
		{"/api/v1/nothing-here", "", http.StatusUnauthorized},
		{"/api/v1/nothing-here", "Bearer " + testSecret, http.StatusNotFound},
	} {
		req, err := http.NewRequest(http.MethodGet, "https://"+address+c.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if c.authorization != "" {
			req.Header.Set("Authorization", c.authorization)
		}
		response, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s over TLS: %v", c.path, err)
		}
		_ = response.Body.Close()
		if response.StatusCode != c.status {
			t.Fatalf("GET %s over TLS = %d, want %d", c.path, response.StatusCode, c.status)
		}
	}

	healthEnv := map[string]string{
		management.PortVariable:        portOf(t, address),
		management.HostVariable:        "127.0.0.1",
		management.TLSCertFileVariable: files.CertFile,
		management.TLSKeyFileVariable:  files.KeyFile,
	}
	if code, log := runCommand(t, healthEnv, "healthcheck"); code != 0 {
		t.Fatalf("healthcheck against serve over TLS exited %d, want 0; log: %s", code, log)
	}
}

// signalChildVariable makes the test binary, run again by
// TestASecondSignalEndsTheProcess, act as the process under test.
const signalChildVariable = "OPAMP_TEST_SIGNAL_CHILD"

// The first SIGINT or SIGTERM starts a graceful stop, and stops catching
// them, so a second one ends the process even when the stop hangs. The child takes signalContext, as main does, and then never
// finishes stopping.
func TestASecondSignalEndsTheProcess(t *testing.T) {
	if os.Getenv(signalChildVariable) == "1" {
		ctx, stop := signalContext()
		defer stop()
		fmt.Println("ready")
		<-ctx.Done()
		fmt.Println("stopping")
		time.Sleep(time.Hour) // a shutdown that never ends
		return
	}

	child := exec.Command(os.Args[0], "-test.run=^TestASecondSignalEndsTheProcess$", "-test.count=1")
	child.Env = append(os.Environ(), signalChildVariable+"=1")
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	lines := make(chan string, 16)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
		exited <- child.Wait()
	}()
	t.Cleanup(func() { _ = child.Process.Kill() })

	// Bounded: a child that hangs before printing fails the test, and the
	// cleanup above kills it.
	waitFor := func(want string) {
		t.Helper()
		deadline := time.After(30 * time.Second)
		for {
			select {
			case line, open := <-lines:
				if !open {
					t.Fatalf("the child never printed %q", want)
				}
				if line == want {
					return
				}
			case <-deadline:
				t.Fatalf("the child didn't print %q within 30 s", want)
			}
		}
	}
	waitFor("ready")
	if err := child.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	waitFor("stopping")

	// Signal until it ends, bounded by attempts: the signal handling is
	// handed back on the context's own goroutine, so the first of these
	// may still be caught; without the hand-back, every one is.
	for attempt := 0; attempt < 500; attempt++ {
		if err := child.Process.Signal(os.Interrupt); err != nil {
			break // it has exited
		}
		select {
		case err := <-exited:
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("the child exited with %v, want killed by SIGINT", err)
			}
			status, ok := exit.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGINT {
				t.Fatalf("the child exited with %v, want killed by SIGINT", err)
			}
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatal("a second SIGINT didn't end the process: the signal handling wasn't handed back")
}

// waitForAgent reads serve's log until it names the agent listener's
// address.
func waitForAgent(t *testing.T, log *syncBuffer) string {
	t.Helper()
	for attempt := 0; attempt < 500; attempt++ {
		for _, line := range logLines(t, log.String()) {
			if line["msg"] == "serving the agent endpoint" {
				return line["address"].(string)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("serve never logged the agent listener's address; log: %s", log.String())
	return ""
}

// serve opens the agent listener beside the management API. On the agent
// port: /health and /ready answer, a poll without a credential is 401, and
// nothing else is served (the debug routes included). The diagnostics are
// on the management port behind the secret. Without its TLS files the agent
// listener says once that a TLS-terminating load balancer is expected; a
// loopback OPAMP_SERVER_URL is warned about once, without its value.
func TestServeOpensTheAgentListenerBesideTheManagementAPI(t *testing.T) {
	d := databasetest.New(t, testDatabase)
	values := serveEnv(d.RuntimeURL)
	values[agent.ServerURLVariable] = "http://localhost:51299/opamp/api/v1"
	env := newTestEnv(values)
	address, log := startServe(t, env)
	agent := waitForAgent(t, log)
	if portOf(t, agent) == portOf(t, address) {
		t.Fatalf("both listeners on %s", agent)
	}
	for _, c := range []struct {
		method, target, authorization string
		status                        int
		body                          string
	}{
		{http.MethodGet, "http://" + agent + "/health", "", http.StatusOK, "ok\n"},
		{http.MethodGet, "http://" + agent + "/ready", "", http.StatusOK, "ready\n"},
		{http.MethodPost, "http://" + agent + "/opamp/api/v1", "", http.StatusUnauthorized, "Unauthorized\n"},
		{http.MethodGet, "http://" + agent + "/opamp/api/v1/debug/pprof/", "Bearer " + testSecret, http.StatusNotFound, "Not Found\n"},
		{http.MethodGet, "http://" + agent + "/api/v1/debug/pprof/", "Bearer " + testSecret, http.StatusNotFound, "Not Found\n"},
		{http.MethodGet, "http://" + address + "/api/v1/debug/pprof/", "", http.StatusUnauthorized, ""},
		{http.MethodGet, "http://" + address + "/api/v1/debug/admission", "", http.StatusUnauthorized, ""},
	} {
		if status, body := request(t, c.method, c.target, c.authorization, ""); status != c.status || (c.body != "" && body != c.body) {
			t.Errorf("%s %s: %d %q, want %d", c.method, c.target, status, body, c.status)
		}
	}
	status, body := request(t, http.MethodGet, "http://"+address+"/api/v1/debug/admission", "Bearer "+testSecret, "")
	var admission map[string]any
	if status != http.StatusOK || json.Unmarshal([]byte(body), &admission) != nil || admission["max_in_flight"] != float64(100) ||
		admission["enabled"] != true {
		t.Fatalf("the admission view: %d %s", status, body)
	}
	if status, body := request(t, http.MethodGet, "http://"+address+"/api/v1/debug/pprof/goroutine?debug=1", "Bearer "+testSecret, ""); status != http.StatusOK ||
		!strings.HasPrefix(body, "goroutine profile:") {
		t.Fatalf("a goroutine profile: %d %.100q", status, body)
	}

	logged := log.String()
	balancer, loopback := 0, 0
	for _, line := range logLines(t, logged) {
		message, _ := line["msg"].(string)
		if strings.Contains(message, "a TLS-terminating load balancer is expected") && line["level"] == "INFO" {
			balancer++
		}
		if strings.Contains(message, "OPAMP_SERVER_URL names a loopback address") && line["level"] == "WARN" {
			loopback++
		}
	}
	if balancer != 1 || loopback != 1 || strings.Contains(logged, "localhost:51299") {
		t.Fatalf("%d load-balancer lines, %d loopback warnings: %s", balancer, loopback, logged)
	}
}

// serve refuses agent settings it can't use before it touches the
// database: one line naming the variable, never its value.
func TestServeRefusesAgentSettingsItCannotUse(t *testing.T) {
	files := testcert.New(t)
	for _, c := range []struct {
		name   string
		values map[string]string
		want   string
	}{
		{"no server URL", map[string]string{agent.ServerURLVariable: ""},
			"Refusing to start: OPAMP_SERVER_URL is not set: the agent endpoint's public URL, which collectors are handed to poll"},
		{"a server URL with a password", map[string]string{agent.ServerURLVariable: "https://someone:password-marker-3b1@opamp.example/opamp/api/v1"},
			"Refusing to start: OPAMP_SERVER_URL must not carry a user name or password"},
		{"a server URL with a query", map[string]string{agent.ServerURLVariable: "https://opamp.example/opamp/api/v1?marker-3b1"},
			"Refusing to start: OPAMP_SERVER_URL must not carry a query"},
		{"a server URL of another scheme", map[string]string{agent.ServerURLVariable: "wss://marker-3b1.example/"},
			"Refusing to start: OPAMP_SERVER_URL must be an http or https URL"},
		{"one agent TLS file", map[string]string{agent.TLSKeyFileVariable: files.KeyFile},
			"Refusing to start: OPAMP_AGENT_TLS_CERT_FILE and OPAMP_AGENT_TLS_KEY_FILE must be set together, or neither (only OPAMP_AGENT_TLS_KEY_FILE is set)"},
		{"an agent host that is a name", map[string]string{agent.HostVariable: "marker-3b1.internal"},
			"Refusing to start: OPAMP_AGENT_HOST must be an IP address, such as 127.0.0.1 or ::1"},
		{"an agent port that isn't one", map[string]string{agent.PortVariable: "marker-3b1"},
			"Refusing to start: OPAMP_AGENT_PORT must be a port number from 0 to 65535"},
		{"a heartbeat of 5 s", map[string]string{agent.HeartbeatIntervalVariable: "5"},
			"Refusing to start: OPAMP_HEARTBEAT_INTERVAL_SECONDS must be a whole number from 10 to 300"},
		{"a concurrency cap that isn't a number", map[string]string{agent.MaxConcurrentRequestsVariable: "marker-3b1"},
			"Refusing to start: OPAMP_MAX_CONCURRENT_REQUESTS must be a whole number from 1 to 10000"},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := serveEnv(unreachableURL)
			for name, value := range c.values {
				env[name] = value
			}
			code, log := runCommand(t, env, "serve")
			lines := logLines(t, log)
			if code != 1 || len(lines) != 1 || lines[0]["msg"] != c.want {
				t.Fatalf("serve exited %d with log %s, want 1 and one line %q", code, log, c.want)
			}
			if strings.Contains(log, "marker-3b1") || strings.Contains(log, "PRIVATE KEY") {
				t.Fatalf("the refusal quotes a value: %s", log)
			}
		})
	}
}
