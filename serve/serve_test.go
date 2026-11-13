package serve

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/awnumar/memguard"

	"github.com/telflo/opamp-server/agent"
	"github.com/telflo/opamp-server/internal/diagnostics"
	"github.com/telflo/opamp-server/internal/shutdown"
	"github.com/telflo/opamp-server/internal/testcert"
	"github.com/telflo/opamp-server/management"
	"github.com/telflo/opamp-server/tokens"
)

// serveHTTP runs the management listener alone, through serveListeners as
// serve runs both: what the listener tests below exercise.
func serveHTTP(ctx context.Context, settings Settings, handler http.Handler, logger *slog.Logger, shutdownTimeout time.Duration) int {
	return serveListeners(ctx, []listenerSpec{managementSpec(settings, handler)}, lifecycle{}, logger, shutdownTimeout)
}

// listening is serveHTTP running until the test ends: its address, its
// log, and what it exited with.
type listening struct {
	address string
	log     *syncBuffer
	cancel  context.CancelFunc
	// done is closed when serveHTTP returns, code then holding its result.
	done chan struct{}
	code int
}

// startServeHTTP runs serveHTTP with settings and handler, and waits for
// the line naming its address.
func startServeHTTP(t *testing.T, settings Settings, handler http.Handler, shutdownTimeout time.Duration) *listening {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	l := &listening{log: &syncBuffer{}, cancel: cancel, done: make(chan struct{})}
	logger := slog.New(slog.NewJSONHandler(l.log, nil))
	go func() {
		defer close(l.done)
		l.code = serveHTTP(ctx, settings, handler, logger, shutdownTimeout)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-l.done:
		case <-time.After(ShutdownTimeout + 5*time.Second):
			t.Error("serveHTTP didn't stop after its context ended")
		}
	})

	// Bounded by attempts, not a fixed wait: each one sleeps briefly.
	for attempt := 0; attempt < 500; attempt++ {
		select {
		case <-l.done:
			t.Fatalf("serveHTTP exited %d before listening; log: %s", l.code, l.log.String())
		default:
		}
		for _, line := range strings.Split(strings.TrimSpace(l.log.String()), "\n") {
			var decoded map[string]any
			if json.Unmarshal([]byte(line), &decoded) == nil && decoded["msg"] == "serving the management API" {
				l.address = decoded["address"].(string)
				return l
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("serveHTTP never logged its address; log: %s", l.log.String())
	return nil
}

// tlsSettings are serve's settings with TLS on, from a certificate made for
// the test, bound to 127.0.0.1 on a free port.
func tlsSettings(t *testing.T) (Settings, testcert.Files) {
	t.Helper()
	files := testcert.New(t)
	config, err := management.LoadTLS(envOf(map[string]string{management.TLSCertFileVariable: files.CertFile, management.TLSKeyFileVariable: files.KeyFile}))
	if err != nil || config == nil {
		t.Fatalf("management.LoadTLS() = %v, %v", config, err)
	}
	secrets, err := management.NewSecrets(testSecret, nil)
	if err != nil {
		t.Fatal(err)
	}
	return Settings{Management: management.Settings{Host: "127.0.0.1", Port: 0, TLS: config, Secrets: secrets}}, files
}

// httpsClient is a client with config's TLS: the roots it trusts and the
// versions it speaks.
func httpsClient(config *tls.Config) *http.Client {
	return &http.Client{Transport: &http.Transport{TLSClientConfig: config, Proxy: nil}}
}

func get(t *testing.T, client *http.Client, url, authorization string) (int, string, error) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = response.Body.Close() }()
	body, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(body), nil
}

// A forced stop that finds no handler running waits for nothing: wait
// answers true at once, whatever its limit.
func TestAnIdleInFlightWaitsForNothing(t *testing.T) {
	started := time.Now()
	if !(&inFlight{}).wait(time.Minute) {
		t.Fatal("wait with no handler running = false")
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("wait with no handler running took %v", elapsed)
	}
}

// With both TLS files set, the management API speaks HTTPS, TLS 1.2 or
// later, on the address OPAMP_MANAGEMENT_HOST names.
func TestServeHTTPSpeaksTLSWithTheConfiguredCertificate(t *testing.T) {
	settings, files := tlsSettings(t)
	l := startServeHTTP(t, settings, apiStandIn(), ShutdownTimeout)
	if !strings.HasPrefix(l.address, "127.0.0.1:") {
		t.Fatalf("serveHTTP listens on %s, want 127.0.0.1 (OPAMP_MANAGEMENT_HOST)", l.address)
	}
	if !strings.Contains(l.log.String(), `"tls":true`) {
		t.Fatalf("the startup line doesn't say TLS is on: %s", l.log.String())
	}
	base := "https://" + l.address
	client := httpsClient(&tls.Config{RootCAs: files.Pool})

	if status, body, err := get(t, client, base+"/health", ""); err != nil || status != http.StatusOK || body != "ok" {
		t.Fatalf("GET /health over TLS = %d %q, %v; want 200 ok", status, body, err)
	}
	list := base + guardedPath
	if status, body, err := get(t, client, list, ""); err != nil || status != http.StatusUnauthorized || body != unauthorized {
		t.Fatalf("the list without the secret = %d %s, %v; want 401", status, body, err)
	}
	if status, body, err := get(t, client, list, "Bearer "+testSecret); err != nil || status != http.StatusOK {
		t.Fatalf("the list with the secret = %d %s, %v; want 200", status, body, err)
	}

	// A client that doesn't trust the certificate stops at the handshake.
	if _, _, err := get(t, httpsClient(&tls.Config{}), base+"/health", ""); err == nil {
		t.Fatal("a client that doesn't trust the certificate was answered")
	}
}

// Plain HTTP on the TLS port gets net/http's 400, never an answer of the
// API's: not /health, not a 401 that would mean the request was read.
func TestServeHTTPRefusesPlainHTTPOnTheTLSPort(t *testing.T) {
	settings, _ := tlsSettings(t)
	l := startServeHTTP(t, settings, apiStandIn(), ShutdownTimeout)
	plain := &http.Client{Transport: &http.Transport{Proxy: nil}}
	for _, path := range []string{"/health", guardedPath} {
		status, body, err := get(t, plain, "http://"+l.address+path, "Bearer "+testSecret)
		if err != nil || status != http.StatusBadRequest || !strings.Contains(body, "HTTP request to an HTTPS server") {
			t.Fatalf("plain HTTP to %s = %d %q, %v; want 400 from the TLS listener", path, status, body, err)
		}
	}
}

// TLS 1.0 and 1.1 are refused at the handshake; 1.2 and 1.3 are served.
func TestServeHTTPRefusesTLSOlderThan1Point2(t *testing.T) {
	settings, files := tlsSettings(t)
	l := startServeHTTP(t, settings, apiStandIn(), ShutdownTimeout)
	for _, c := range []struct {
		name     string
		min, max uint16
		served   bool
	}{
		{"TLS 1.0", tls.VersionTLS10, tls.VersionTLS10, false},
		{"TLS 1.1", tls.VersionTLS11, tls.VersionTLS11, false},
		{"TLS 1.2", tls.VersionTLS12, tls.VersionTLS12, true},
		{"TLS 1.3", tls.VersionTLS13, tls.VersionTLS13, true},
	} {
		client := httpsClient(&tls.Config{RootCAs: files.Pool, MinVersion: c.min, MaxVersion: c.max})
		status, _, err := get(t, client, "https://"+l.address+"/health", "")
		if c.served && (err != nil || status != http.StatusOK) {
			t.Errorf("%s: %d, %v; want 200", c.name, status, err)
		}
		if !c.served && err == nil {
			t.Errorf("%s was answered %d, want the handshake refused", c.name, status)
		}
	}
}

// Without the TLS files the listener speaks plain HTTP, as before, and the
// startup line says so.
func TestServeHTTPSpeaksPlainHTTPWithoutTLSFiles(t *testing.T) {
	settings, _ := tlsSettings(t)
	settings.Management.TLS = nil
	l := startServeHTTP(t, settings, healthOnly(), ShutdownTimeout)
	plain := &http.Client{Transport: &http.Transport{Proxy: nil}}
	if status, body, err := get(t, plain, "http://"+l.address+"/health", ""); err != nil || status != http.StatusOK || body != "ok" {
		t.Fatalf("GET /health = %d %q, %v; want 200 ok", status, body, err)
	}
	if !strings.Contains(l.log.String(), `"tls":false`) {
		t.Fatalf("the startup line doesn't say TLS is off: %s", l.log.String())
	}
}

// guardedPath is apiStandIn's route behind the secret.
const guardedPath = "/api/v1/guarded"

// unauthorized is apiStandIn's 401 body.
const unauthorized = `{"error":{"code":"unauthorized","message":"Missing or invalid credentials."}}`

// apiStandIn stands in for a component's handler where only the listener
// is under test: /health, and guardedPath behind the test secret (401
// without it). The management API's own authentication is package
// management's to test.
func apiStandIn() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", health)
	mux.HandleFunc("GET "+guardedPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testSecret {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, unauthorized)
			return
		}
		_, _ = io.WriteString(w, "[]")
	})
	return mux
}

func healthOnly() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", health)
	return mux
}

// A request still running when the shutdown timeout passes is cut off: its
// context is cancelled (which ends its database query) and its connection
// closed, and serve exits 1 rather than hang.
func TestServeHTTPCutsOffRequestsStillRunningAfterTheShutdownTimeout(t *testing.T) {
	settings, _ := tlsSettings(t)
	settings.Management.TLS = nil
	started := make(chan struct{})
	cancelled := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done() // a query that never answers
		close(cancelled)
	})

	// No time at all to finish: Shutdown gives up at once while the
	// request runs, so the cut-off path is taken without a wait.
	l := startServeHTTP(t, settings, handler, 0)
	requestDone := make(chan error, 1)
	go func() {
		plain := &http.Client{Transport: &http.Transport{Proxy: nil}}
		_, _, err := get(t, plain, "http://"+l.address+"/stuck", "")
		requestDone <- err
	}()
	<-started
	l.cancel()

	select {
	case <-l.done:
		if l.code != 1 {
			t.Fatalf("serveHTTP exited %d, want 1 after cutting a request off", l.code)
		}
	case <-time.After(ShutdownTimeout + 5*time.Second):
		t.Fatal("serveHTTP didn't stop: the stuck request held it")
	}
	// Close cancels the request's context, but the handler's goroutine
	// sees it in its own time: wait for it, with a bound.
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the stuck request's context wasn't cancelled")
	}
	select {
	case <-requestDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the cut-off request's client never returned")
	}
	if !strings.Contains(l.log.String(), "requests were still running when the shutdown timeout passed") {
		t.Fatalf("the log doesn't say requests were cut off: %s", l.log.String())
	}
}

// A port that can't be listened on is a refusal naming it.
func TestServeHTTPRefusesAPortInUse(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()
	settings, _ := tlsSettings(t)
	settings.Management.Port = taken.Addr().(*net.TCPAddr).Port
	var log syncBuffer
	code := serveHTTP(context.Background(), settings, healthOnly(), slog.New(slog.NewJSONHandler(&log, nil)), ShutdownTimeout)
	if code != 1 || !strings.Contains(log.String(), "can't listen on the management port") {
		t.Fatalf("serveHTTP exited %d with log %s", code, log.String())
	}
}

// destroyRecorder is serveWith's destroy, recording each keyring it is given
// and then destroying it as serve's own does.
type destroyRecorder struct {
	mu        sync.Mutex
	destroyed []*tokens.Keyring
}

func (r *destroyRecorder) destroy(keys *tokens.Keyring) {
	r.mu.Lock()
	r.destroyed = append(r.destroyed, keys)
	r.mu.Unlock()
	destroyKeyring(keys)
}

// Serve destroys the enrollment tokens' keys on every way out: a setting
// refused after the keys were read, a database URL that doesn't parse, a
// database that refuses the role, a listener that can't open, and a
// graceful stop. Each way passes exactly one keyring to destroy, which
// mints nothing afterwards: without a destroy on one of the ways, its case
// fails.
func TestServeDestroysTheKeysOnEveryWayOut(t *testing.T) {
	d := testDB(t)
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()
	takenPort := strconv.Itoa(taken.Addr().(*net.TCPAddr).Port)

	env := func(databaseURL string, changes map[string]string) func(string) string {
		values := map[string]string{
			DatabaseURLVariable:       databaseURL,
			management.HostVariable:   "127.0.0.1",
			management.PortVariable:   "0",
			management.SecretVariable: testSecret,
			EncryptionKeyVariable:     testEncryptionKey,
			agent.HostVariable:        "127.0.0.1",
			agent.PortVariable:        "0",
			agent.ServerURLVariable:   testServerURL,
		}
		for name, value := range changes {
			values[name] = value
		}
		return envOf(values)
	}
	for _, c := range []struct {
		name string
		env  func(string) string
		// stop cancels serve once it listens; the others end on their own.
		stop bool
		code int
		log  string
	}{
		{"a setting refused after the keys", env(d.RuntimeURL, map[string]string{management.PortVariable: "65536"}), false, 1, "OPAMP_MANAGEMENT_PORT"},
		{"a database URL that doesn't parse", env("postgresql://someone:a-secret@localhost:notaport/telflo_opamp_server", nil), false, 1, "is not a valid PostgreSQL connection URL"},
		{"a database that refuses the role", env(d.OwnerURL, nil), false, 1, "is telflo_opamp_server_owner or a member of it"},
		{"a listener that can't open", env(d.RuntimeURL, map[string]string{management.PortVariable: takenPort}), false, 1, "can't listen on the management port"},
		{"the agent listener that can't open", env(d.RuntimeURL, map[string]string{agent.PortVariable: takenPort}), false, 1, "can't listen on the agent port"},
		{"a graceful stop", env(d.RuntimeURL, nil), true, 0, "stopped"},
	} {
		t.Run(c.name, func(t *testing.T) {
			recorder := &destroyRecorder{}
			log := &syncBuffer{}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			exited := make(chan int, 1)
			go func() {
				exited <- serveWith(ctx, RoleAll, c.env, unsetNothing, slog.New(slog.NewJSONHandler(log, nil)), recorder.destroy, nil)
			}()
			if c.stop {
				// Bounded by attempts, not a fixed wait.
				for attempt := 0; !strings.Contains(log.String(), "serving the management API"); attempt++ {
					if attempt == 500 {
						t.Fatalf("serve never listened; log: %s", log.String())
					}
					time.Sleep(20 * time.Millisecond)
				}
				cancel()
			}
			var code int
			select {
			case code = <-exited:
			case <-time.After(ShutdownTimeout + 30*time.Second):
				t.Fatal("serve didn't return")
			}
			if code != c.code || !strings.Contains(log.String(), c.log) {
				t.Fatalf("serve exited %d with log %s, want %d and %q", code, log.String(), c.code, c.log)
			}
			recorder.mu.Lock()
			destroyed := append([]*tokens.Keyring(nil), recorder.destroyed...)
			recorder.mu.Unlock()
			if len(destroyed) != 1 || destroyed[0] == nil {
				t.Fatalf("serve destroyed %d keyrings, want its one", len(destroyed))
			}
			if _, err := destroyed[0].Mint(tokens.Claims{}); !errors.Is(err, tokens.ErrNoKey) {
				t.Fatalf("the keyring still mints after serve returned: %v", err)
			}
		})
	}
}

// Serve's way of destroying the keys destroys the keyring and then purges
// every locked buffer the process holds.
func TestServeDestroysTheKeysThenPurgesEveryLockedBuffer(t *testing.T) {
	keys, err := tokens.NewKeyring(testEncryptionKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	other := memguard.NewBuffer(32)
	destroyAndPurge(keys)
	if other.IsAlive() {
		t.Fatal("a locked buffer outside the keyring survived the purge")
	}
	if _, err := keys.Mint(tokens.Claims{}); !errors.Is(err, tokens.ErrNoKey) {
		t.Fatalf("the keyring still mints: %v", err)
	}
}

// The longest profile or trace the diagnostics serve ends before a
// listener's write timeout would cut its reply off.
func TestTheProfileCapIsUnderTheWriteTimeout(t *testing.T) {
	if diagnostics.MaxProfileSeconds >= writeTimeout.Seconds() {
		t.Fatalf("the cap %d s isn't under the write timeout %s", diagnostics.MaxProfileSeconds, writeTimeout)
	}
}

// health answers 200 "ok", as the management API's /health does.
func health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok"))
}

// Every request's context carries the forced stop (shutdown.ForcedStop),
// which a caller hanging up doesn't cancel and a forced stop does: what
// the management API's long deletes rely on to outlive their caller and
// still be cut off with every other request (package management tests the
// deletes' side of it).
func TestTheListenersCarryAForcedStopOnlyAForcedStopCancels(t *testing.T) {
	settings, _ := tlsSettings(t)
	settings.Management.TLS = nil
	type seen struct {
		request, stop context.Context
	}
	arrived := make(chan seen, 2)
	handler := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		stop, ok := shutdown.ForcedStop(r.Context())
		if !ok {
			t.Error("a request's context carries no forced stop")
			return
		}
		arrived <- seen{r.Context(), stop}
		<-stop.Done() // work that outlives its caller, until a forced stop
	})
	// No time at all to finish: the stop is a forced one.
	l := startServeHTTP(t, settings, handler, 0)
	send := func(ctx context.Context) {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+l.address+"/work", nil)
		if err != nil {
			t.Error(err)
			return
		}
		if response, err := (&http.Client{Transport: &http.Transport{Proxy: nil}}).Do(request); err == nil {
			_ = response.Body.Close()
		}
	}

	ctx, hangUp := context.WithCancel(context.Background())
	go send(ctx)
	first := <-arrived
	hangUp()
	select {
	case <-first.request.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("the server never saw the caller hang up")
	}
	if first.stop.Err() != nil {
		t.Fatal("a caller hanging up cancelled the forced stop")
	}

	l.cancel()
	select {
	case <-first.stop.Done():
	case <-time.After(CloseWait + 30*time.Second):
		t.Fatal("the forced stop never cancelled the request's forced-stop context")
	}
	<-l.done
	if l.code != 1 {
		t.Fatalf("serve exited %d, want 1 after cutting a request off", l.code)
	}
}
