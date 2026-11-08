package middleware

import (
	"context"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/telflo/opamp-server/telemetry"
)

// DatabaseDeadline bounds an agent request's database work: the token
// lookup, the organization's limits, and everything after the body is read
// (the poll). A store that doesn't answer within it fails the request with
// 503 and a Retry-After of 10 to 30 seconds.
const DatabaseDeadline = 5 * time.Second

// Deadline gives the rest of the request d to do its database work: its
// context's deadline, which pgx honours.
func Deadline(d time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ServerSpan makes an agent request's server span: named
// "POST /opamp/api/v1", with the method, the route and the status, and an
// error status at 500 or above; the stack and the poll stamp opamp.outcome
// on it. Never the path, the query, a header or the body; and no log line (a
// successful poll writes none). A client's trace context isn't taken up: a
// collector doesn't choose this server's trace ids.
func ServerSpan(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, span := telemetry.Tracer().Start(r.Context(), http.MethodPost+" "+AgentRoute,
			trace.WithNewRoot(),
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("http.request.method", http.MethodPost),
				attribute.String("http.route", AgentRoute),
			))
		defer span.End()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r.WithContext(ctx))
		span.SetAttributes(attribute.Int("http.response.status_code", recorder.status))
		if recorder.status >= http.StatusInternalServerError {
			span.SetStatus(codes.Error, "")
		}
	})
}

// statusRecorder remembers the status a handler answered with.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if !r.wroteHeader {
		r.status = status
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	r.wroteHeader = true
	return r.ResponseWriter.Write(p)
}

// Unwrap lets http.ResponseController reach the connection's writer.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}
