package telemetry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func attrMap(attrs []attribute.KeyValue) map[string]string {
	m := make(map[string]string, len(attrs))
	for _, a := range attrs {
		m[string(a.Key)] = a.Value.String()
	}
	return m
}

// A "<table>.<verb>" operation must yield the semconv 1.37 collection and
// operation names (so a collector's set_semconv_span_name renders
// "<verb> <table>" rather than the bare db.system), alongside both spellings
// of db.system for older transforms.
func TestDBSpanAttributes_TableAndVerb(t *testing.T) {
	got := attrMap(dbSpanAttributes("client_instances.ensure"))
	want := map[string]string{
		"db.system":          "postgresql",
		"db.system.name":     "postgresql",
		"db.collection.name": "client_instances",
		"db.operation.name":  "ensure",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d attributes, want %d: %v", len(got), len(want), got)
	}
}

// Operations without a dot (or with a missing half) set only what they can and
// never emit an empty attribute value.
func TestDBSpanAttributes_Degenerate(t *testing.T) {
	cases := map[string]map[string]string{
		"ping":    {"db.operation.name": "ping"},
		"":        {},
		".ensure": {"db.operation.name": "ensure"},
		"tokens.": {"db.collection.name": "tokens"},
	}
	for op, extra := range cases {
		got := attrMap(dbSpanAttributes(op))
		if got["db.system"] != "postgresql" || got["db.system.name"] != "postgresql" {
			t.Errorf("%q: db.system attributes missing: %v", op, got)
		}
		for k, v := range extra {
			if got[k] != v {
				t.Errorf("%q: %s = %q, want %q", op, k, got[k], v)
			}
		}
		if len(got) != 2+len(extra) {
			t.Errorf("%q: got %v, want exactly %d extra attributes", op, got, len(extra))
		}
	}
}

// StartDBSpan's spans are named "db.<operation>".
func TestStartDBSpanIsNamedForTheDatabase(t *testing.T) {
	recorder := recordSpans(t)

	_, end := StartDBSpan(context.Background(), "publications.put")
	end(nil)

	spans := recorder.Ended()
	if len(spans) != 1 || spans[0].Name() != "db.publications.put" {
		t.Fatalf("spans = %v, want one named db.publications.put", spans)
	}
}

// recordSpans installs a tracer provider whose ended spans the test reads.
func recordSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(previous) })
	return recorder
}

// A failed database call's span says which operation failed and, for a
// Postgres error, its SQLSTATE, and nothing of the error's text: that can
// quote a SQL argument (a YAML, an email) or a connection's details. Not in
// the status, an attribute or an event.
func TestStartDBSpanDescribesAFailureByOperationAndSQLStateOnly(t *testing.T) {
	const marker = "marker-yaml-receivers-otlp"
	cases := []struct {
		name        string
		err         error
		description string
		sqlState    string
	}{
		{
			name: "a Postgres error",
			err: fmt.Errorf("publications: put: %w", &pgconn.PgError{
				Severity: "ERROR", Code: "23514", Message: "new row violates check: " + marker,
				Detail: "Failing row contains (" + marker + ").", Where: marker,
			}),
			description: "publications.put failed (SQLSTATE 23514)",
			sqlState:    "23514",
		},
		{
			name:        "any other error",
			err:         errors.New("publications: put: dial postgres://telflo_opamp:" + marker + "@db/telflo_opamp"),
			description: "publications.put failed",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			recorder := recordSpans(t)
			_, end := StartDBSpan(context.Background(), "publications.put")
			end(c.err)

			spans := recorder.Ended()
			if len(spans) != 1 {
				t.Fatalf("%d spans, want 1", len(spans))
			}
			span := spans[0]
			if span.Status().Code.String() != "Error" || span.Status().Description != c.description {
				t.Fatalf("status = %v %q, want Error %q", span.Status().Code, span.Status().Description, c.description)
			}
			attributes := attrMap(span.Attributes())
			if attributes["db.response.status_code"] != c.sqlState {
				t.Errorf("db.response.status_code = %q, want %q", attributes["db.response.status_code"], c.sqlState)
			}
			everything := []string{span.Status().Description}
			for k, v := range attributes {
				everything = append(everything, k+"="+v)
			}
			for _, event := range span.Events() {
				everything = append(everything, event.Name)
				for k, v := range attrMap(event.Attributes) {
					everything = append(everything, k+"="+v)
				}
			}
			for _, field := range everything {
				if strings.Contains(field, marker) || strings.Contains(field, "postgres://") {
					t.Fatalf("the span holds the error's text: %q", field)
				}
			}
		})
	}
}
