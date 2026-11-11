package management

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"runtime/debug"
	"strings"

	"github.com/google/uuid"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/telflo/telflo-opamp-server/store/apikeys"
	"github.com/telflo/telflo-opamp-server/telemetry"
)

// MaxBodyBytes caps every request body: 2 MiB.
const MaxBodyBytes = 2 << 20

// bearerScheme is the Authorization scheme the API takes, matched without
// regard to case (RFC 9110 section 11.1).
const bearerScheme = "Bearer"

// authenticate lets a request through only with `Authorization: Bearer
// <credential>` naming an accepted secret, or an API key that is neither
// revoked nor expired; every other request gets the one 401. A key's
// request carries it in its context (apiKeyFrom), for authorize, and its
// id in the request's log line; the key's last use is set to now. Nothing
// it reads from the header reaches a log line, a span or an error. A
// store that fails answers 500, as any route's would.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r.Header)
		if !ok {
			writeUnauthorized(w)
			return
		}
		if s.secrets.Accepts(token) {
			next.ServeHTTP(w, r)
			return
		}
		id, sum, ok := apikeys.Parse(token)
		if !ok {
			writeUnauthorized(w)
			return
		}
		stored, err := s.apiKeys.Lookup(r.Context(), id)
		switch {
		case errors.Is(err, apikeys.ErrNotFound):
			writeUnauthorized(w)
			return
		case err != nil:
			s.storeFailed(w, r, storeAPIKeys, err)
			return
		}
		now := s.clock()
		if !stored.Matches(sum) || !stored.ValidAt(now) {
			writeUnauthorized(w)
			return
		}
		if err := s.apiKeys.Touch(r.Context(), stored.ID, now); err != nil {
			s.storeFailed(w, r, storeAPIKeys, err)
			return
		}
		if note, ok := r.Context().Value(requestNoteKey{}).(*requestNote); ok {
			note.apiKeyID = stored.ID
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), apiKeyKey{}, stored.Key)))
	})
}

// apiKeyKey is the context key of the API key a request authenticated
// with; a request on the secret has none.
type apiKeyKey struct{}

// apiKeyFrom is the API key the request authenticated with, if it did.
func apiKeyFrom(ctx context.Context) (apikeys.Key, bool) {
	k, ok := ctx.Value(apiKeyKey{}).(apikeys.Key)
	return k, ok
}

// requestNoteKey is the context key of a request's requestNote.
type requestNoteKey struct{}

// requestNote is what the handlers learn that observe's log line names: the
// API key a request authenticated with (uuid.Nil for the secret).
type requestNote struct {
	apiKeyID uuid.UUID
}

// The answers authorize gives a key on a route it may not call.
const (
	messageSecretOnly       = "This route takes the management secret, not an API key."
	messageAllOrganizations = "Registering an organization takes the management secret, or an API key for every organization."
	messageSetLimits        = "Changing an organization's limits takes the management secret, or an API key for every organization."
	codeForbidden           = errorCode("forbidden")
)

// pathWildcard is a wildcard of a route's pattern: {name}.
var pathWildcard = regexp.MustCompile(`\{([A-Za-z]+)\}`)

// authorize wraps each route of the API with what an API key may call on
// it; a request on the secret passes every route. By the route's pattern:
//
//   - a route under /api/v1/organizations/{organizationId} opens to a key
//     for that organization; for another key it answers the same 404 as an
//     organization that isn't registered (not_found on the organization's
//     own routes, organization_not_found under it), once every path id is
//     a UUID (a path id that isn't is left to the route's 400), so a key
//     learns nothing of organizations it doesn't hold;
//   - but PUT …/limits, which sets the guards that keep one organization
//     from taking the agent pods its neighbours share, opens to a key for
//     every organization only, and answers another key for the
//     organization 403;
//   - POST /api/v1/organizations opens to a key for every organization, and
//     answers another key 403;
//   - the catch-all, which answers 404, opens to every caller;
//   - every other route (the API keys' own, the diagnostics, and any route
//     added without a rule here) answers a key 403: the secret only.
func (s *Server) authorize(pattern string, next http.Handler) http.Handler {
	_, path, _ := strings.Cut(pattern, " ")
	switch {
	case pattern == apiPrefix:
		return next
	case strings.HasPrefix(path, organizationPath):
		setsLimits := pattern == http.MethodPut+" "+limitsPath
		var wildcards []string
		for _, match := range pathWildcard.FindAllStringSubmatch(path, -1) {
			wildcards = append(wildcards, match[1])
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			k, isKey := apiKeyFrom(r.Context())
			if !isKey {
				next.ServeHTTP(w, r)
				return
			}
			for _, name := range wildcards {
				if _, ok := parseUUID(r.PathValue(name)); !ok {
					next.ServeHTTP(w, r)
					return
				}
			}
			organizationID, _ := parseUUID(r.PathValue("organizationId"))
			switch {
			case !k.Opens(organizationID) && path == organizationPath:
				writeNotFound(w)
				return
			case !k.Opens(organizationID):
				writeOrganizationNotFound(w)
				return
			case setsLimits && !k.AllOrganizations:
				writeError(w, http.StatusForbidden, codeForbidden, messageSetLimits)
				return
			}
			next.ServeHTTP(w, r)
		})
	case pattern == http.MethodPost+" "+organizationsPath:
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if k, isKey := apiKeyFrom(r.Context()); isKey && !k.AllOrganizations {
				writeError(w, http.StatusForbidden, codeForbidden, messageAllOrganizations)
				return
			}
			next.ServeHTTP(w, r)
		})
	default:
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, isKey := apiKeyFrom(r.Context()); isKey {
				writeError(w, http.StatusForbidden, codeForbidden, messageSecretOnly)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// bearerToken returns the token of the request's one Authorization header,
// if that header uses the Bearer scheme and has a non-empty token. Two
// Authorization headers are refused rather than guessed between.
func bearerToken(header http.Header) (string, bool) {
	values := header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	scheme, token, found := strings.Cut(values[0], " ")
	if !found || !strings.EqualFold(scheme, bearerScheme) {
		return "", false
	}
	token = strings.TrimLeft(token, " ")
	if token == "" {
		return "", false
	}
	return token, true
}

// limitBody caps the request body at MaxBodyBytes before anything parses
// it: a declared length over the cap answers 413 at once, and a body that
// runs over it while being read (chunked) fails readBody with a 413.
func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > MaxBodyBytes {
			writeError(w, http.StatusRequestEntityTooLarge, codePayloadTooLarge, messagePayloadTooLarge)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
		next.ServeHTTP(w, r)
	})
}

// readBody reads a request body limitBody has capped. On failure it has
// already answered (413 over the cap, 400 otherwise) and returns false. The
// handlers read their bodies through it.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, codePayloadTooLarge, messagePayloadTooLarge)
		} else {
			writeError(w, http.StatusBadRequest, codeInvalidRequest, messageUnreadableBody)
		}
		return nil, false
	}
	return body, true
}

// recoverPanics answers a handler's panic with 500 internal_error and no
// detail. The log line names the route and the panic value's type, never
// the value, which could hold anything the handler held.
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}
			if recovered == http.ErrAbortHandler {
				panic(recovered)
			}
			s.logger.Error("a handler panicked",
				"route", s.routePattern(r),
				"panic", typeName(recovered),
				"stack", string(debug.Stack()))
			if recorder, ok := w.(*statusRecorder); ok && recorder.wroteHeader {
				// Too late for a 500: end the response.
				panic(http.ErrAbortHandler)
			}
			writeError(w, http.StatusInternalServerError, codeInternalError, messageInternalError)
		}()
		next.ServeHTTP(w, r)
	})
}

// observe logs each request and makes its server span:
// method, route pattern, status and duration, and the id of the API key it
// authenticated with (apiKeyId), if it did. Never a header, a body, the
// path (its other ids) or the query.
func (s *Server) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := s.clock()
		method := methodName(r.Method)
		route := s.routePattern(r)

		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := telemetry.Tracer().Start(ctx, method+" "+route,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(
				attribute.String("http.request.method", method),
				attribute.String("http.route", route),
			))
		defer span.End()

		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		note := &requestNote{}
		next.ServeHTTP(recorder, r.WithContext(context.WithValue(ctx, requestNoteKey{}, note)))

		span.SetAttributes(attribute.Int("http.response.status_code", recorder.status))
		if recorder.status >= 500 {
			span.SetStatus(codes.Error, "")
		}
		attributes := []any{
			"method", method,
			"route", route,
			"status", recorder.status,
			"durationMs", float64(s.clock().Sub(start).Microseconds()) / 1000,
		}
		if note.apiKeyID != uuid.Nil {
			span.SetAttributes(attribute.String("opamp.api_key.id", note.apiKeyID.String()))
			attributes = append(attributes, "apiKeyId", note.apiKeyID.String())
		}
		s.logger.Info("request", attributes...)
	})
}

// methodName is the request's method when it is a standard one, else
// "_OTHER" (OpenTelemetry's convention), so a client's made-up method never
// reaches a log line or a span.
func methodName(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodConnect, http.MethodOptions, http.MethodTrace:
		return method
	}
	return "_OTHER"
}

// typeName names a panic value's type (%T) without formatting the value.
func typeName(value any) string {
	return fmt.Sprintf("%T", value)
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
