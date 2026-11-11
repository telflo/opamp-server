package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/telflo/telflo-opamp-server/store/organizations"
	"github.com/telflo/telflo-opamp-server/store/publications"
)

const (
	testSecret     = "the-current-secret-0123456789abcdef"
	testPrevious   = "the-previous-secret-0123456789abcd"
	unauthorized   = `{"error":{"code":"unauthorized","message":"Missing or invalid credentials."}}`
	notFoundBody   = `{"error":{"code":"not_found","message":"Not found."}}`
	tooLargeBody   = `{"error":{"code":"payload_too_large","message":"The request body is larger than 2 MiB."}}`
	internalBody   = `{"error":{"code":"internal_error","message":"Internal server error."}}`
	probeRoute     = "/api/v1/probe/{id}"
	readingRoute   = "/api/v1/reading"
	panickingRoute = "/api/v1/panicking"
)

// syncBuffer is a log the handler's goroutines and the test can share.
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

// noOrganizations is a registry that fails the test if anything calls it:
// the server's own routes and /health never reach a store here.
type noOrganizations struct{ t *testing.T }

func (s noOrganizations) fail() error {
	s.t.Helper()
	s.t.Error("the organizations store was called")
	return errors.New("no store")
}

func (s noOrganizations) Create(context.Context, organizations.Organization) (organizations.CreateResult, organizations.Organization, error) {
	return 0, organizations.Organization{}, s.fail()
}

func (s noOrganizations) Get(context.Context, uuid.UUID) (organizations.Organization, error) {
	return organizations.Organization{}, s.fail()
}

func (s noOrganizations) Exists(context.Context, uuid.UUID) (bool, error) {
	return false, s.fail()
}

func (s noOrganizations) Rename(context.Context, uuid.UUID, string, time.Time) (organizations.Organization, error) {
	return organizations.Organization{}, s.fail()
}

func (s noOrganizations) Delete(context.Context, uuid.UUID) (bool, error) {
	return false, s.fail()
}

func (s noOrganizations) Limits(context.Context, uuid.UUID) (organizations.Limits, error) {
	return organizations.Limits{}, s.fail()
}

func (s noOrganizations) SetLimits(context.Context, uuid.UUID, organizations.Limits) (organizations.Limits, error) {
	return organizations.Limits{}, s.fail()
}

// noStore is a publications store that fails the test if anything calls
// it: the server's own routes and /health never reach a store here.
type noStore struct{ t *testing.T }

func (s noStore) fail() error {
	s.t.Helper()
	s.t.Error("the publications store was called")
	return errors.New("no store")
}

func (s noStore) Put(context.Context, publications.Publication) (publications.PutResult, publications.Publication, error) {
	return 0, publications.Publication{}, s.fail()
}

func (s noStore) Get(context.Context, uuid.UUID, uuid.UUID) (publications.Publication, error) {
	return publications.Publication{}, s.fail()
}

func (s noStore) List(context.Context, uuid.UUID, uuid.UUID, int, *publications.Cursor) (publications.Page, error) {
	return publications.Page{}, s.fail()
}

func (s noStore) Delete(context.Context, uuid.UUID, uuid.UUID) (bool, error) {
	return false, s.fail()
}

func (s noStore) DeleteForConfiguration(context.Context, uuid.UUID, uuid.UUID) (int, error) {
	return 0, s.fail()
}

func (s noStore) Usage(context.Context, uuid.UUID, uuid.UUID) ([]publications.Usage, error) {
	return nil, s.fail()
}

// testServer is a Server with the test secret current and testPrevious
// still accepted, no database (a store that fails the test if called), a
// log the test reads, and three test routes
// beside the real ones: GET probeRoute answers 200, PUT readingRoute reads
// its body and answers its length, and panickingRoute panics with a message
// holding the body.
func testServer(t *testing.T) (*Server, *syncBuffer) {
	t.Helper()
	secrets, err := NewSecrets(testSecret, []string{testPrevious})
	if err != nil {
		t.Fatal(err)
	}
	log := &syncBuffer{}
	fleetStores := postgresFleetStores(testDB(t).Runtime)
	s, err := New(Options{
		Secrets:          secrets,
		Organizations:    noOrganizations{t},
		Publications:     noStore{t},
		Fleets:           fleetStores.fleets,
		CollectorGroups:  fleetStores.groups,
		EnrollmentTokens: fleetStores.tokens,
		Collectors:       fleetStores.collectors,
		Variables:        fleetStores.variables,
		APIKeys:          fleetStores.apiKeys,
		Keys:             testKeys(t),
		Clock:            time.Now,
		Logger:           slog.New(slog.NewJSONHandler(log, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	s.api.handle("GET "+probeRoute, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("probe"))
	}))
	s.api.handle("PUT "+readingRoute, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := readBody(w, r)
		if !ok {
			return
		}
		_, _ = w.Write([]byte(strconv.Itoa(len(body))))
	}))
	s.api.handle(panickingRoute, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		panic("the handler held " + string(body))
	}))
	return s, log
}

func serve(s *Server, r *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	s.Handler().ServeHTTP(recorder, r)
	return recorder
}

func withAuthorization(r *http.Request, values ...string) *http.Request {
	for _, value := range values {
		r.Header.Add("Authorization", value)
	}
	return r
}

func assertAnswer(t *testing.T, label string, got *httptest.ResponseRecorder, status int, body string) {
	t.Helper()
	if got.Code != status || got.Body.String() != body {
		t.Fatalf("%s: %d %s, want %d %s", label, got.Code, got.Body.String(), status, body)
	}
	if strings.HasPrefix(body, "{") && got.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("%s: Content-Type %q, want application/json", label, got.Header().Get("Content-Type"))
	}
}

func TestEveryBadAuthorizationAnswersTheSame401(t *testing.T) {
	s, _ := testServer(t)
	lastByte := testSecret[:len(testSecret)-1] + "g"
	cases := map[string][]string{
		"no header":                    nil,
		"Basic":                        {"Basic dXNlcjpwYXNz"},
		"Basic with the secret":        {"Basic " + testSecret},
		"Bearer with nothing":          {"Bearer"},
		"Bearer and spaces":            {"Bearer   "},
		"a wrong secret":               {"Bearer a-wrong-secret-0123456789abcdefgh"},
		"differing in its last byte":   {"Bearer " + lastByte},
		"the secret without a scheme":  {testSecret},
		"a prefix of the secret":       {"Bearer " + testSecret[:16]},
		"the secret and more":          {"Bearer " + testSecret + "x"},
		"two Authorization headers":    {"Bearer " + testSecret, "Bearer " + testSecret},
		"a scheme glued to the secret": {"Bearer" + testSecret},
	}
	for label, values := range cases {
		for _, target := range []string{"/api/v1/probe/1", "/api/v1/unknown", "/api/"} {
			got := serve(s, withAuthorization(httptest.NewRequest(http.MethodGet, target, nil), values...))
			assertAnswer(t, label+" "+target, got, http.StatusUnauthorized, unauthorized)
			if got.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatalf("%s: WWW-Authenticate %q, want Bearer", label, got.Header().Get("WWW-Authenticate"))
			}
		}
	}
}

func TestTheCurrentAndThePreviousSecretAreAccepted(t *testing.T) {
	s, _ := testServer(t)
	for _, authorization := range []string{
		"Bearer " + testSecret,
		"Bearer " + testPrevious,
		"bearer " + testSecret, // the scheme is case-insensitive
		"BEARER " + testPrevious,
		"Bearer  " + testSecret, // extra spaces before the token
	} {
		got := serve(s, withAuthorization(httptest.NewRequest(http.MethodGet, "/api/v1/probe/1", nil), authorization))
		assertAnswer(t, authorization[:7], got, http.StatusOK, "probe")
	}
}

// After authentication, any /api/ request no route matches, by path or by
// method, is a 404 in the envelope. Without the secret it is the 401, so
// an unauthenticated caller learns nothing about routes.
func TestAnUnknownAPIRouteIs401WithoutTheSecretAnd404WithIt(t *testing.T) {
	s, _ := testServer(t)
	cases := []struct{ method, target string }{
		{http.MethodGet, "/api/"},
		{http.MethodGet, "/api/v1/nothing"},
		{http.MethodDelete, "/api/v2/organizations/x/publications/y"},
		{http.MethodPost, "/api/v1/probe/1"}, // a registered path, the wrong method
		{http.MethodPut, "/api/v1/probe/1"},
		{http.MethodGet, readingRoute}, // only PUT is registered
	}
	for _, c := range cases {
		label := c.method + " " + c.target
		assertAnswer(t, label+" without the secret",
			serve(s, httptest.NewRequest(c.method, c.target, nil)), http.StatusUnauthorized, unauthorized)
		assertAnswer(t, label+" with the secret",
			serve(s, withAuthorization(httptest.NewRequest(c.method, c.target, nil), "Bearer "+testSecret)),
			http.StatusNotFound, notFoundBody)
	}
}

func TestOutsideTheAPIAnythingButHealthIs404(t *testing.T) {
	s, _ := testServer(t)
	for _, target := range []string{"/", "/opamp/api/v1", "/healthz"} {
		assertAnswer(t, target, serve(s, httptest.NewRequest(http.MethodGet, target, nil)), http.StatusNotFound, notFoundBody)
	}
	assertAnswer(t, "POST /health", serve(s, httptest.NewRequest(http.MethodPost, "/health", nil)), http.StatusNotFound, notFoundBody)
}

// /health needs no secret and no database: the server's store here fails
// the test if anything calls it.
func TestHealthAnswersWithoutTheSecretOrADatabase(t *testing.T) {
	s, log := testServer(t)
	if _, ok := s.publications.(noStore); !ok {
		t.Fatal("the test server has a store")
	}
	assertAnswer(t, "GET /health", serve(s, httptest.NewRequest(http.MethodGet, "/health", nil)), http.StatusOK, "ok")
	assertAnswer(t, "GET /health with a wrong secret",
		serve(s, withAuthorization(httptest.NewRequest(http.MethodGet, "/health", nil), "Bearer wrong")), http.StatusOK, "ok")
	if log.String() != "" {
		t.Fatalf("/health logged: %s", log.String())
	}
}

func TestAPanicAnswers500WithoutDetail(t *testing.T) {
	s, log := testServer(t)
	request := withAuthorization(httptest.NewRequest(http.MethodPost, panickingRoute, strings.NewReader("panic-marker-91c2")), "Bearer "+testSecret)
	assertAnswer(t, "a panic", serve(s, request), http.StatusInternalServerError, internalBody)
	logged := log.String()
	if !strings.Contains(logged, `"msg":"a handler panicked"`) || !strings.Contains(logged, `"status":500`) {
		t.Fatalf("the panic wasn't logged: %s", logged)
	}
	if strings.Contains(logged, "panic-marker-91c2") || strings.Contains(logged, "the handler held") {
		t.Fatalf("the log holds the panic's value: %s", logged)
	}
}

// chunked hides a body's length, as a chunked request does.
type chunked struct{ io.Reader }

func TestABodyOver2MiBAnswers413(t *testing.T) {
	s, _ := testServer(t)
	over := strings.Repeat("y", MaxBodyBytes+1)

	// A declared length over the cap: refused before anything reads it.
	declared := withAuthorization(httptest.NewRequest(http.MethodPut, readingRoute, strings.NewReader(over)), "Bearer "+testSecret)
	if declared.ContentLength != MaxBodyBytes+1 {
		t.Fatalf("ContentLength = %d", declared.ContentLength)
	}
	assertAnswer(t, "2 MiB + 1, declared", serve(s, declared), http.StatusRequestEntityTooLarge, tooLargeBody)

	// No declared length: refused when reading passes the cap.
	undeclared := withAuthorization(httptest.NewRequest(http.MethodPut, readingRoute, chunked{strings.NewReader(over)}), "Bearer "+testSecret)
	undeclared.ContentLength = -1
	assertAnswer(t, "2 MiB + 1, chunked", serve(s, undeclared), http.StatusRequestEntityTooLarge, tooLargeBody)

	// Exactly 2 MiB is read whole (the handler answers the length it read).
	exact := withAuthorization(httptest.NewRequest(http.MethodPut, readingRoute, chunked{strings.NewReader(over[:MaxBodyBytes])}), "Bearer "+testSecret)
	exact.ContentLength = -1
	assertAnswer(t, "2 MiB", serve(s, exact), http.StatusOK, strconv.Itoa(MaxBodyBytes))

	// Authentication comes first: no secret, no reading, a 401.
	unauthenticated := httptest.NewRequest(http.MethodPut, readingRoute, strings.NewReader(over))
	assertAnswer(t, "2 MiB + 1 without the secret", serve(s, unauthenticated), http.StatusUnauthorized, unauthorized)
}

// Requests with the right and wrong secrets and a body holding a marker
// YAML; the log holds neither the secrets nor the marker, nor the path's
// ids, and each line names only the method, the route pattern, the status
// and the duration.
func TestTheLogHoldsNoSecretHeaderBodyOrPath(t *testing.T) {
	s, log := testServer(t)
	const marker = "receivers:\n  otlp: # yaml-marker-5d1e"
	const wrong = "a-wrong-secret-0123456789abcdefgh"
	type call struct{ method, target, authorization string }
	calls := []call{
		{http.MethodPut, readingRoute, "Bearer " + testSecret},
		{http.MethodPut, readingRoute, "Bearer " + testPrevious},
		{http.MethodPut, readingRoute, "Bearer " + wrong},
		{http.MethodGet, "/api/v1/organizations/org-id-3b7/publications/pub-id-8c1", "Bearer " + testSecret},
		{http.MethodGet, "/api/v1/probe/probe-id-2a9?cursor=cursor-value-4f0", "Bearer " + testSecret},
		{http.MethodPost, panickingRoute, "Bearer " + testSecret},
		{"BREW", "/api/v1/probe/1", "Bearer " + testSecret},
		{http.MethodGet, "/not-api/path-id-6e5", ""},
	}
	for _, c := range calls {
		request := httptest.NewRequest(c.method, c.target, strings.NewReader(marker))
		request.Header.Set("X-Custom-Header", "header-marker-0a4")
		request.Header.Set("Cookie", "session=cookie-marker-1b2")
		if c.authorization != "" {
			request.Header.Set("Authorization", c.authorization)
		}
		serve(s, request)
	}

	logged := log.String()
	for _, leaked := range []string{
		testSecret, testPrevious, wrong, "Bearer", "Authorization", "yaml-marker-5d1e", "receivers",
		"header-marker-0a4", "cookie-marker-1b2", "org-id-3b7", "pub-id-8c1", "probe-id-2a9",
		"cursor-value-4f0", "path-id-6e5", "BREW",
	} {
		if strings.Contains(logged, leaked) {
			t.Errorf("the log holds %q", leaked)
		}
	}

	var requests int
	for _, line := range strings.Split(strings.TrimSpace(logged), "\n") {
		var fields map[string]any
		if err := json.Unmarshal([]byte(line), &fields); err != nil {
			t.Fatalf("log line %q isn't JSON: %v", line, err)
		}
		if fields["msg"] != "request" {
			continue
		}
		requests++
		keys := make([]string, 0, len(fields))
		for key := range fields {
			keys = append(keys, key)
		}
		for _, key := range keys {
			switch key {
			case "time", "level", "msg", "method", "route", "status", "durationMs":
			default:
				t.Errorf("a request's log line has %q: %s", key, line)
			}
		}
	}
	if requests != len(calls) {
		t.Fatalf("logged %d requests, want %d: %s", requests, len(calls), logged)
	}
	for _, want := range []string{
		`"route":"` + readingRoute + `"`, `"route":"` + probeRoute + `"`, `"route":"/api/"`,
		`"route":"/"`, `"method":"_OTHER"`, `"status":401`, `"status":500`,
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("the log lacks %s: %s", want, logged)
		}
	}
}

// The server span carries the route pattern, the method and the status, and
// continues an inbound trace; no header or body.
func TestTheServerSpanNamesTheRouteAndNothingElse(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previousProvider, previousPropagator := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(previousProvider)
		otel.SetTextMapPropagator(previousPropagator)
	})

	s, _ := testServer(t)
	request := withAuthorization(httptest.NewRequest(http.MethodGet, "/api/v1/probe/probe-id-2a9", strings.NewReader("yaml-marker-5d1e")), "Bearer "+testSecret)
	request.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	serve(s, request)

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("%d spans, want 1", len(spans))
	}
	span := spans[0]
	if span.Name() != "GET "+probeRoute {
		t.Fatalf("span name %q", span.Name())
	}
	if span.Parent().TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("the span didn't continue the inbound trace: parent %v", span.Parent())
	}
	attributes := map[string]string{}
	for _, attribute := range span.Attributes() {
		attributes[string(attribute.Key)] = attribute.Value.String()
	}
	want := map[string]string{
		"http.request.method":       "GET",
		"http.route":                probeRoute,
		"http.response.status_code": "200",
	}
	if len(attributes) != len(want) {
		t.Fatalf("span attributes %v, want %v", attributes, want)
	}
	for key, value := range want {
		if attributes[key] != value {
			t.Fatalf("span attributes %v, want %v", attributes, want)
		}
	}
}

func TestNewRefusesMissingOptions(t *testing.T) {
	secrets, err := NewSecrets(testSecret, nil)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	// Stores no case calls: New only checks that they were given.
	var store publications.Store = noStore{t}
	var registry organizations.Store = noOrganizations{t}
	for label, options := range map[string]Options{
		"no secrets":             {Organizations: registry, Publications: store, Clock: time.Now, Logger: logger},
		"no organizations store": {Secrets: secrets, Publications: store, Clock: time.Now, Logger: logger},
		"no publications store":  {Secrets: secrets, Organizations: registry, Clock: time.Now, Logger: logger},
		"no clock":               {Secrets: secrets, Organizations: registry, Publications: store, Logger: logger},
		"no logger":              {Secrets: secrets, Organizations: registry, Publications: store, Clock: time.Now},
	} {
		if _, err := New(options); err == nil {
			t.Errorf("%s: New accepted it", label)
		}
	}
}
