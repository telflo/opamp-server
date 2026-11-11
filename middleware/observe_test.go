package middleware

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/telflo/telflo-opamp-server/store/enrollmenttokens"
)

// deadlineLookup records whether its context carried a deadline, and how
// far off, and answers its row or fails as a store past its deadline does.
type deadlineLookup struct {
	row      enrollmenttokens.Authenticated
	exceeded bool

	mu       sync.Mutex
	deadline time.Duration
	had      bool
}

func (l *deadlineLookup) Lookup(ctx context.Context, _ uuid.UUID) (enrollmenttokens.Authenticated, error) {
	deadline, had := ctx.Deadline()
	l.mu.Lock()
	l.had, l.deadline = had, time.Until(deadline)
	l.mu.Unlock()
	if l.exceeded {
		return enrollmenttokens.Authenticated{}, fmt.Errorf("enrollmenttokens.lookup: %w", context.DeadlineExceeded)
	}
	return l.row, nil
}

// An agent request's database work has a deadline of DatabaseDeadline: the
// token lookup's context carries one, and so does the context the poll
// (after the body) runs with; a lookup that runs past it answers 503 with a
// Retry-After of 10 to 30 seconds, an ERROR line.
func TestTheDatabaseWorkHasAFiveSecondDeadline(t *testing.T) {
	k := keyring(t, currentKeySetting)
	token, organization, fleet := uuid.New(), uuid.New(), uuid.New()
	clock := newTestClock()
	lookup := &deadlineLookup{row: activeRow(token, organization, fleet)}
	stack := testStack(k, lookup, DefaultMaxConcurrentRequests, clock)
	var handlerDeadline time.Duration
	handlerHad := false
	h := stack.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadline, had := r.Context().Deadline()
		handlerHad, handlerDeadline = had, time.Until(deadline)
		w.WriteHeader(http.StatusOK)
	}))
	bearer := "Bearer " + mint(t, k, token, fleet)
	req := httptest.NewRequest(http.MethodPost, AgentRoute, strings.NewReader("x"))
	req.Header.Set("Authorization", bearer)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("a poll: %d", rec.Code)
	}
	if !lookup.had || lookup.deadline <= 0 || lookup.deadline > DatabaseDeadline || DatabaseDeadline != 5*time.Second {
		t.Fatalf("the lookup's deadline: %v, %v", lookup.had, lookup.deadline)
	}
	if !handlerHad || handlerDeadline <= 0 || handlerDeadline > DatabaseDeadline {
		t.Fatalf("the poll's deadline: %v, %v", handlerHad, handlerDeadline)
	}

	logs := &strings.Builder{}
	late := &deadlineLookup{exceeded: true}
	a := NewAuthenticator(k, late, clock.Now, captureLogs(logs))
	rec, seen := send(a, bearer)
	retry, _ := strconv.Atoi(rec.Header().Get("Retry-After"))
	if rec.Code != http.StatusServiceUnavailable || seen != nil || retry < UnavailableRetryAfterMin || retry > UnavailableRetryAfterMax {
		t.Fatalf("a lookup past its deadline: %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if lines := logLines(t, logs); len(lines) != 1 || lines[0]["level"] != "ERROR" || lines[0]["outcome"] != outcomeLookupFailed {
		t.Fatalf("its lines: %v", lines)
	}
}

// The stack makes each agent request's server span, "POST /opamp/api/v1"
// with the method, the route, the status and the outcome, an error status
// at 500 or above, and nothing of the path, the query or a header.
func TestTheStackMakesTheRequestsServerSpan(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(previous) })

	k := keyring(t, currentKeySetting)
	clock := newTestClock()
	ok := testStack(k, staticLookup{err: enrollmenttokens.ErrNotFound}, DefaultMaxConcurrentRequests, clock)
	failing := testStack(k, &deadlineLookup{exceeded: true}, DefaultMaxConcurrentRequests, clock)
	bearer := "Bearer " + mint(t, k, uuid.New(), uuid.New())
	for _, stack := range []AgentStack{ok, failing} {
		req := httptest.NewRequest(http.MethodPost, AgentRoute+"?query-marker=1", strings.NewReader("x"))
		req.Header.Set("Authorization", bearer)
		req.Header.Set("User-Agent", "user-agent-marker")
		stack.Handler(http.NotFoundHandler()).ServeHTTP(httptest.NewRecorder(), req)
	}
	spans := recorder.Ended()
	if len(spans) != 2 {
		t.Fatalf("%d spans", len(spans))
	}
	for i, want := range []struct {
		status  int64
		outcome string
		failed  bool
	}{{http.StatusUnauthorized, outcomeUnknownToken, false}, {http.StatusServiceUnavailable, outcomeLookupFailed, true}} {
		span := spans[i]
		got := map[string]string{}
		for _, attr := range span.Attributes() {
			got[string(attr.Key)] = attr.Value.String()
		}
		if span.Name() != "POST /opamp/api/v1" || got["http.request.method"] != "POST" || got["http.route"] != AgentRoute ||
			got["http.response.status_code"] != strconv.FormatInt(want.status, 10) || got["opamp.outcome"] != want.outcome ||
			(span.Status().Code == codes.Error) != want.failed {
			t.Errorf("span %d: %s %v %v", i, span.Name(), got, span.Status())
		}
		if text := fmt.Sprint(span.Attributes(), span.Events()); strings.Contains(text, "marker") || strings.Contains(text, "Bearer") {
			t.Errorf("span %d holds request text: %s", i, text)
		}
	}
}
