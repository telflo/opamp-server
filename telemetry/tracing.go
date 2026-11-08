package telemetry

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// tracerName is the instrumentation-scope name for this service's own spans:
// the management API's server spans and the database spans below.
const tracerName = "github.com/telflo/opamp-server"

// Tracer is the tracer for this service's own spans. When telemetry is not
// configured the global tracer provider is a no-op.
func Tracer() trace.Tracer {
	return otel.Tracer(tracerName)
}

// StartDBSpan opens a client-kind child span, named "db.<operation>", for a
// database call (the stores use pgx, which has no OpenTelemetry
// instrumentation of its own here). Usage:
//
//	ctx, end := telemetry.StartDBSpan(ctx, "publications.put")
//	defer func() { end(err) }()
//
// The returned end marks the span failed when err is non-nil, describing it
// only by the operation and, for a Postgres error, its SQLSTATE (also the
// db.response.status_code attribute): never err's text, which may quote a
// SQL argument (a YAML, an email) or a connection's details. For the same
// reason it records no exception event. When telemetry is not configured
// the global tracer provider is a no-op, so this adds effectively no cost.
//
// operation is "<table>.<verb>" (e.g. "publications.put"). It is split into
// the semconv 1.37 database attributes db.collection.name and
// db.operation.name so a collector's set_semconv_span_name renders the span
// as "<verb> <table>" instead of collapsing every call to the bare
// "postgresql" (which is what happens when only db.system is present).
func StartDBSpan(ctx context.Context, operation string, attrs ...attribute.KeyValue) (context.Context, func(error)) {
	ctx, span := otel.Tracer(tracerName).Start(
		ctx,
		"db."+operation,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(dbSpanAttributes(operation)...),
	)
	if len(attrs) > 0 {
		span.SetAttributes(attrs...)
	}
	return ctx, func(err error) {
		if err != nil {
			description := operation + " failed"
			if sqlState := sqlStateOf(err); sqlState != "" {
				span.SetAttributes(attribute.String("db.response.status_code", sqlState))
				description += " (SQLSTATE " + sqlState + ")"
			}
			span.SetStatus(codes.Error, description)
		}
		span.End()
	}
}

// sqlStateOf is the SQLSTATE of the Postgres error err wraps, or "" when it
// wraps none. Only the five-character code is taken from it.
func sqlStateOf(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// dbSpanAttributes maps a "<table>.<verb>" operation to the semconv database
// attributes. Both the legacy db.system and the 1.37 db.system.name are set so
// older and newer collector transforms recognise the span as a DB call. An
// operation without a dot (or with an empty half) sets only what it can.
func dbSpanAttributes(operation string) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String("db.system", "postgresql"),
		attribute.String("db.system.name", "postgresql"),
	}
	table, verb, found := strings.Cut(operation, ".")
	if !found {
		if operation != "" {
			attrs = append(attrs, attribute.String("db.operation.name", operation))
		}
		return attrs
	}
	if table != "" {
		attrs = append(attrs, attribute.String("db.collection.name", table))
	}
	if verb != "" {
		attrs = append(attrs, attribute.String("db.operation.name", verb))
	}
	return attrs
}
