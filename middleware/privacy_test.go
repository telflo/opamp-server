package middleware

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/telflo/telflo-opamp-server/store/fleets"
	. "github.com/telflo/telflo-opamp-server/store/storetest"
)

// No bearer, key, key setting or Authorization header reaches a log line or
// a span (its name, attributes, status or events), through every way the
// authentication and the limits answer: a success, each refusal, a lost
// credential, the limits' 429s. The ids may (and do) appear, so the capture
// isn't empty. On Postgres too, so the lookup's database span is searched.
func TestTheAgentSidesAuthenticationAndLimitsLogAndTraceNoBearerOrKey(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(previous) })

	Each(t, testDatabase, func(t *testing.T, s *Stores) {
		c := s.CreateFleet(t, OrgA, "edge-prod", fleets.TypeServer)
		other := s.CreateFleet(t, OrgA, "edge-dev", fleets.TypeServer)
		k := keyring(t, currentKeySetting, previousKeySetting)
		stranger := keyring(t, strangerKeySetting)
		a := newAuth(t, s, k)
		limiter := NewOrganizationLimiter(&limitsStub{rpm: map[uuid.UUID]*int32{OrgA: ptr32(30)}}, 7, a.clock.Now, captureLogs(a.logs))
		guard := NewCollectorGuard(1, a.clock.Now, captureLogs(a.logs))
		handler := a.auth.Middleware()(limiter.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Lose") != "" {
				a.auth.RefuseLost(w, r)
				return
			}
			if !guard.Allow(OrgA, uuid.Nil) {
				guard.Refuse(w, r)
				return
			}
			w.WriteHeader(http.StatusOK)
		})))
		tracer := otel.Tracer("privacy-test")

		valid := mint(t, k, c.RotateTokenID, c.Fleet.ID)
		bearers := []string{
			valid,
			mint(t, k, c.DefaultTokenID, c.Fleet.ID),
			mint(t, stranger, c.RotateTokenID, c.Fleet.ID),
			mint(t, k, c.RotateTokenID, other.Fleet.ID),
			mint(t, k, uuid.New(), c.Fleet.ID),
			base64.StdEncoding.EncodeToString([]byte("bearer-canary-plaintext-0b3f-and-padding")),
		}
		codes := map[int]bool{}
		send := func(authorization string, lose bool) {
			req := httptest.NewRequest(http.MethodPost, AgentRoute, nil)
			if authorization != "" {
				req.Header.Set("Authorization", authorization)
			}
			if lose {
				req.Header.Set("X-Lose", "1")
			}
			ctx, span := tracer.Start(req.Context(), "POST "+AgentRoute, trace.WithSpanKind(trace.SpanKindServer))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req.WithContext(ctx))
			span.End()
			codes[rec.Code] = true
		}
		for _, bearer := range bearers {
			send("Bearer "+bearer, false)
			send("Basic "+bearer, false)
		}
		for range 40 {
			send("Bearer "+valid, false) // the guard's 429s (past 20), then the limiter's (past 30)
		}
		send("Bearer "+valid, true) // a lost credential
		send("", false)
		for _, code := range []int{http.StatusOK, http.StatusUnauthorized, http.StatusTooManyRequests} {
			if !codes[code] {
				t.Fatalf("no request answered %d: %v", code, codes)
			}
		}

		logs := a.logs.String()
		if !strings.Contains(logs, c.RotateTokenID.String()) || !strings.Contains(logs, outcomeFleetMismatch) {
			t.Fatalf("the logs hold no refusal: the capture is broken:\n%s", logs)
		}
		var spans strings.Builder
		for _, span := range recorder.Ended() {
			fmt.Fprintf(&spans, "%s %v %v %v\n", span.Name(), span.Attributes(), span.Status(), span.Events())
		}
		if !strings.Contains(spans.String(), "opamp.outcome") {
			t.Fatalf("no span holds an outcome: the capture is broken:\n%s", spans.String())
		}
		forbidden := append([]string{currentKeySetting, previousKeySetting, strangerKeySetting,
			"bearer-canary", "Bearer", "Basic", "Authorization", "authorization"}, bearers...)
		for _, written := range []struct{ where, text string }{{"the logs", logs}, {"the spans", spans.String()}} {
			for _, secret := range forbidden {
				if strings.Contains(written.text, secret) {
					t.Errorf("%s hold %q", written.where, secret)
				}
			}
		}
	})
}
