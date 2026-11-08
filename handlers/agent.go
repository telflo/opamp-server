// Package handlers is the agent endpoint's poll: POST /opamp/api/v1 for a
// server-class fleet's collectors. It holds the default-token hand-off, the
// rotate-token poll, the YAML, the reported fields, the stuck threshold and
// the health gate, the push and the reconcile, and the rotation push, on
// the fleet model's stores: the YAML cache keyed by organization and
// SHA-256; an empty YAML never pushed; enrollment without a race (the
// store's); no hot loop for an unassigned collector; a re-delivery that is
// recorded as one; the acknowledgement's InstanceUid the raw 16 bytes; a
// database failure answered 503, never 400; no error's text, YAML,
// attribute value or name in a log line, a span or an answer. A collector
// nothing resolves for runs its own file and is sent no configuration.
//
// The middleware before it (middleware.AgentStack, built by management's
// serve; its comment gives the layers' order and why) authenticates the
// bearer into a middleware.Principal, applies the limits and reads the body
// through the cap; the poll decodes the AgentToServer once, checks the
// per-collector guard when the stack didn't, and answers.
package handlers

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/open-telemetry/opamp-go/protobufs"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"

	"github.com/telflo/opamp-server/middleware"
	"github.com/telflo/opamp-server/store/collectors"
	"github.com/telflo/opamp-server/store/enrollmenttokens"
	"github.com/telflo/opamp-server/store/orgvariables"
	"github.com/telflo/opamp-server/tokens"
)

// RotateTokens is what the hand-off reads: enrollmenttokens.Store's
// GetActiveRotate, the fleet's current rotate token valid at now.
type RotateTokens interface {
	GetActiveRotate(ctx context.Context, organizationID, fleetID uuid.UUID, now time.Time) (enrollmenttokens.Token, error)
}

// Options are what the poll needs. serve (management's newReplica) builds
// them from its settings: the Postgres stores, the keyring from OPAMP_ENCRYPTION_KEY,
// the Authenticator and CollectorGuard of the same middleware.AgentStack,
// OPAMP_SERVER_URL and OPAMP_HEARTBEAT_INTERVAL_SECONDS.
type Options struct {
	Collectors collectors.Store
	Tokens     RotateTokens
	// Variables resolve the organization variables a served configuration's
	// ${org:NAME} references are filled in with (orgvariables.Render), for
	// the collector's group's and pin's variable groups: serve passes the
	// variables store.
	Variables orgvariables.Store
	// Keyring mints the rotate token's bearer pushed in connection
	// settings (its current key).
	Keyring *tokens.Keyring
	// Authenticator answers a credential lost mid-poll (RefuseLost): the
	// same one that authenticated the request.
	Authenticator *middleware.Authenticator
	// Guard is the per-collector guard, checked once the body names the
	// collector.
	Guard *middleware.CollectorGuard
	// ServerURL is OPAMP_SERVER_URL: the connection settings' destination
	// endpoint, where a collector polls once handed a token.
	ServerURL string
	// HeartbeatInterval is OPAMP_HEARTBEAT_INTERVAL_SECONDS, pushed with
	// the connection settings (the OpAMP client refuses an offer without
	// one).
	HeartbeatInterval time.Duration
	// Cache is the YAML cache; nil makes the poll its own.
	Cache *YamlCache
	// VariablesCache is the variables cache, the pod's own; nil makes the
	// poll its own.
	VariablesCache *VariablesCache
	// Now is the injected clock.
	Now func() time.Time
	// Logger takes one line per refused or failed poll.
	Logger *slog.Logger
}

// AgentHandler answers the agent's poll. Safe for concurrent use: it keeps
// nothing per request but the YAML and variables caches, which are.
type AgentHandler struct {
	collectors collectors.Store
	tokens     RotateTokens
	variables  orgvariables.Store
	keyring    *tokens.Keyring
	auth       *middleware.Authenticator
	guard      *middleware.CollectorGuard
	serverURL  string
	heartbeat  time.Duration
	cache      *YamlCache
	vars       *VariablesCache
	// forbidden spaces the 403s' log lines: a line a minute per outcome
	// and organization, with the count.
	forbidden *middleware.ForbiddenSampler
	now       func() time.Time
	logger    *slog.Logger
}

// NewAgentHandler returns the poll, refusing Options missing a part.
func NewAgentHandler(o Options) (*AgentHandler, error) {
	switch {
	case o.Collectors == nil || o.Tokens == nil || o.Variables == nil:
		return nil, errors.New("handlers: a store is missing")
	case o.Keyring == nil || o.Authenticator == nil || o.Guard == nil:
		return nil, errors.New("handlers: the keyring, the authenticator or the collector guard is missing")
	case o.ServerURL == "":
		return nil, errors.New("handlers: the server's URL is missing")
	case o.HeartbeatInterval < time.Second:
		return nil, errors.New("handlers: the heartbeat interval is under a second")
	case o.Now == nil || o.Logger == nil:
		return nil, errors.New("handlers: the clock or the logger is missing")
	}
	cache := o.Cache
	if cache == nil {
		cache = NewYamlCache()
	}
	vars := o.VariablesCache
	if vars == nil {
		vars = NewVariablesCache()
	}
	logger := o.Logger
	return &AgentHandler{
		collectors: o.Collectors, tokens: o.Tokens, variables: o.Variables,
		keyring: o.Keyring, auth: o.Authenticator, guard: o.Guard,
		serverURL: o.ServerURL, heartbeat: o.HeartbeatInterval, cache: cache, vars: vars,
		forbidden: middleware.NewForbiddenSampler(o.Now, func(f middleware.ForbiddenFlush) {
			logger.Warn("agent poll refused", append([]any{"route", middleware.AgentRoute, "status", http.StatusForbidden}, f.Attrs()...)...)
		}),
		now: o.Now, logger: logger,
	}, nil
}

// The outcomes a poll records: opamp.outcome on its span, and the outcome
// of the log line a refused or failed poll writes.
const (
	outcomePushed            = "config_pushed"
	outcomeRedelivered       = "config_redelivered"
	outcomeReconciled        = "reconciled"
	outcomeAck               = "ack"
	outcomeFullState         = "full_state_requested"
	outcomeEmptyYAML         = "empty_configuration"
	outcomeHandOff           = "default_token_handoff"
	outcomeHandOffNoSettings = "default_token_no_connection_settings"
	outcomeSuspended         = "default_token_suspended"
	// outcomeNotAccepted: a configuration resolved for an agent that doesn't
	// accept one (no AcceptsRemoteConfig): nothing pushed or recorded served.
	outcomeNotAccepted = "remote_config_not_accepted"
	// outcomeDescriptionAsked: a collector the organization doesn't hold
	// polled without an agent description: asked for its full state,
	// enrolled by the poll that carries one, so the group it reports is
	// always checked.
	outcomeDescriptionAsked = "full_state_before_enrollment"
	// outcomeRenderedTooLarge: the resolved configuration filled in with
	// its organization variables is over orgvariables.MaxRenderedBytes:
	// nothing pushed or recorded served, one error row per filled-in hash.
	outcomeRenderedTooLarge = "rendered_too_large"

	outcomeMethod         = "invalid_method"
	outcomeNoPrincipal    = "missing_principal"
	outcomeUnreadable     = "body_unreadable"
	outcomeTooLarge       = "body_too_large"
	outcomeInvalidMessage = "invalid_message"
	outcomeInvalidUID     = "invalid_instance_uid"
	// outcomeOverBudget: the body holds more fields, or deeper nesting, than
	// withinMessageBudget allows, so it isn't decoded: 413, like a body
	// over the cap.
	outcomeOverBudget = "message_over_budget"
	// outcomeInstanceUIDMismatch: the body's instance uid isn't the one its
	// InstanceUIDHeader names, which the guard was checked by.
	outcomeInstanceUIDMismatch = "instance_uid_mismatch"
	outcomeUnknownKind         = "unknown_token_kind"
	outcomeFleetMismatch       = "collector_fleet_mismatch"
	outcomeForeignGroup        = "foreign_group"
	outcomeNoGroup             = "no_group"
	outcomeCollectorLimit      = "collector_limit_reached"
	outcomeConfigurationGone   = "configuration_gone"
	outcomeHashMismatch        = "configuration_hash_mismatch"
	outcomeCollectorVanished   = "collector_vanished"
	outcomeKeysUnavailable     = "keys_unavailable"
	outcomeStoreFailed         = "store_failed"
	outcomeEncodeFailed        = "encode_failed"
)

// The poll's own refusals.
var (
	// errFleetMismatch: the organization's collector of this instance uid
	// is in another fleet than the token's.
	errFleetMismatch = errors.New("handlers: the collector is another fleet's")
	// errForeignGroup: the group the collector names isn't a group id.
	errForeignGroup = errors.New("handlers: the collector names no group of its fleet")
	// errNoGroup: a new collector's description names no group
	// (telflo.collector_group_id absent or blank): refused, never enrolled
	// into the Default group.
	errNoGroup = errors.New("handlers: the collector names no group")
	// errConfigurationGone: what Poll resolved was deleted before its YAML
	// was read (or before the served write); the next poll resolves again.
	errConfigurationGone = errors.New("handlers: the resolved configuration is gone")
	// errHashMismatch: the stored YAML's SHA-256 isn't the one resolved.
	errHashMismatch = errors.New("handlers: the stored configuration's SHA-256 doesn't match")
	// errCollectorVanished: enrolled, then not found by the next Poll.
	errCollectorVanished = errors.New("handlers: the collector vanished after enrolling")
	// errDescriptionNeeded: the organization holds no collector of the
	// instance uid, and the message carries no agent description to enroll
	// it by: its poll asks for the full state instead.
	errDescriptionNeeded = errors.New("handlers: no agent description to enroll the collector by")
)

// poll is one request's state.
type poll struct {
	r   *http.Request
	p   middleware.Principal
	msg *protobufs.AgentToServer
	uid uuid.UUID
	now time.Time
	// serverName is the ServerName header, trimmed: the name a new
	// collector asks for.
	serverName string
}

// ServeHTTP answers one poll.
func (h *AgentHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.refuse(w, r, http.StatusBadRequest, outcomeMethod, nil)
		return
	}
	p, ok := middleware.PrincipalFrom(r.Context())
	if !ok || p.OrganizationID == uuid.Nil || p.FleetID == uuid.Nil || p.TokenID == uuid.Nil {
		h.refuse(w, r, http.StatusUnauthorized, outcomeNoPrincipal, nil)
		return
	}
	// BodyLimit has already read the body through the cap, so in the
	// server this read can't exceed it. The handler caps it anyway so it
	// stays bounded when called without the stack, as its tests do.
	body, err := io.ReadAll(io.LimitReader(r.Body, middleware.MaxBodyBytes+1))
	switch {
	case err != nil:
		h.refuse(w, r, http.StatusBadRequest, outcomeUnreadable, nil)
		return
	case len(body) > middleware.MaxBodyBytes:
		h.refuse(w, r, http.StatusRequestEntityTooLarge, outcomeTooLarge, nil)
		return
	}
	msg := &protobufs.AgentToServer{}
	// Decoding multiplies a body's structure in memory, so it is counted
	// first: a body under the cap can still hold millions of empty fields.
	if !withinMessageBudget(body, msg.ProtoReflect().Descriptor()) {
		h.refuse(w, r, http.StatusRequestEntityTooLarge, outcomeOverBudget, nil)
		return
	}
	if err := proto.Unmarshal(body, msg); err != nil {
		h.refuse(w, r, http.StatusBadRequest, outcomeInvalidMessage, nil)
		return
	}
	// The OpAMP specification's instance uid is 16 bytes, never its 36
	// characters of text.
	uid, err := uuid.FromBytes(msg.InstanceUid)
	if len(msg.InstanceUid) != 16 || err != nil {
		h.refuse(w, r, http.StatusBadRequest, outcomeInvalidUID, nil)
		return
	}
	// The guard was checked before the organization's rate by the header's
	// instance uid, when the request carries one (see
	// middleware.AgentStack): the body's must be the same. Without the
	// header, it is checked here.
	if header, present, valid := middleware.HeaderInstanceUID(r); present && (!valid || header != uid) {
		h.refuse(w, r, http.StatusBadRequest, outcomeInstanceUIDMismatch, nil)
		return
	}
	if _, guarded := middleware.GuardedInstanceUID(r.Context()); !guarded && !h.guard.Allow(p.OrganizationID, uid) {
		h.guard.Refuse(w, r)
		return
	}
	q := &poll{r: r, p: p, msg: msg, uid: uid, now: h.now(), serverName: trimmedServerName(r)}

	var answer *protobufs.ServerToAgent
	var outcome string
	switch p.TokenKind {
	case enrollmenttokens.KindDefault:
		answer, outcome, err = h.handOff(r.Context(), q)
	case enrollmenttokens.KindRotate:
		answer, outcome, err = h.serve(r.Context(), q)
	default:
		h.refuse(w, r, http.StatusUnauthorized, outcomeUnknownKind, nil)
		return
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	encoded, err := proto.Marshal(answer)
	if err != nil {
		h.refuse(w, r, http.StatusInternalServerError, outcomeEncodeFailed, nil)
		return
	}
	setOutcome(r.Context(), outcome)
	header := w.Header()
	header.Set("Content-Type", "application/x-protobuf")
	// An answer may carry a bearer (the connection settings): no cache may
	// keep it.
	header.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}

// fail answers a poll whose work failed or was refused by a store: a
// credential lost mid-poll 401 (the Authenticator's), a collector of
// another fleet, a foreign group, no group or the cap 403 with fixed
// bodies, and anything else, a database failure first, 503 with a jittered
// Retry-After.
func (h *AgentHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case middleware.CredentialLost(err):
		h.auth.RefuseLost(w, r)
	case errors.Is(err, errFleetMismatch), errors.Is(err, collectors.ErrFleetMismatch):
		h.refuse(w, r, http.StatusForbidden, outcomeFleetMismatch, nil)
	case errors.Is(err, errForeignGroup), errors.Is(err, collectors.ErrForeignGroup):
		h.refuse(w, r, http.StatusForbidden, outcomeForeignGroup, nil)
	case errors.Is(err, errNoGroup):
		h.refuse(w, r, http.StatusForbidden, outcomeNoGroup, nil)
	case errors.Is(err, collectors.ErrCapReached):
		h.refuse(w, r, http.StatusForbidden, outcomeCollectorLimit, nil)
	case errors.Is(err, errConfigurationGone):
		h.refuse(w, r, http.StatusServiceUnavailable, outcomeConfigurationGone, err)
	case errors.Is(err, errHashMismatch):
		h.refuse(w, r, http.StatusServiceUnavailable, outcomeHashMismatch, err)
	case errors.Is(err, errCollectorVanished):
		h.refuse(w, r, http.StatusServiceUnavailable, outcomeCollectorVanished, err)
	case errors.Is(err, tokens.ErrNoKey):
		h.refuse(w, r, http.StatusServiceUnavailable, outcomeKeysUnavailable, err)
	default:
		h.refuse(w, r, http.StatusServiceUnavailable, outcomeStoreFailed, err)
	}
}

// refuse answers a refused or failed poll, records its outcome on the span
// and writes its one log line (a 403's sampled, a line a minute per outcome
// and organization with the count): the route, the status, the outcome, the
// ids the Principal holds, and the SQLSTATE of a database failure. Never an
// error's text: a store's error may quote a value. A poll whose client went
// away (its context cancelled: the supervisor cancels its in-flight
// default-token poll when it applies the hand-off) is request_cancelled, at
// Info, whatever failed: nothing failed on the server's side.
func (h *AgentHandler) refuse(w http.ResponseWriter, r *http.Request, status int, outcome string, err error) {
	level, message := slog.LevelWarn, "agent poll refused"
	switch {
	case middleware.ClientGone(r.Context()):
		status, outcome = http.StatusServiceUnavailable, middleware.OutcomeCancelled
		level, message = slog.LevelInfo, "agent poll abandoned by its client"
	case status >= http.StatusInternalServerError:
		level, message = slog.LevelError, "agent poll failed"
	}
	setOutcome(r.Context(), outcome)
	attrs := []any{"route", middleware.AgentRoute, "status", status, "outcome", outcome}
	if p, ok := middleware.PrincipalFrom(r.Context()); ok {
		attrs = append(attrs, "organization_id", p.OrganizationID.String(),
			"fleet_id", p.FleetID.String(), "token_id", p.TokenID.String())
	}
	if state := sqlState(err); state != "" {
		attrs = append(attrs, "sqlstate", state)
	}
	if status == http.StatusForbidden {
		// A collector of another fleet, a foreign group, no group, the cap: its
		// supervisor keeps polling through a 403, so a line a minute per
		// outcome and organization, with the count.
		var organization uuid.UUID
		if p, ok := middleware.PrincipalFrom(r.Context()); ok {
			organization = p.OrganizationID
		}
		if log, count, full := h.forbidden.Note(outcome, organization); log {
			h.logger.Log(r.Context(), level, message, append(attrs, middleware.ForbiddenCountAttrs(count, full)...)...)
		}
	} else {
		h.logger.Log(r.Context(), level, message, attrs...)
	}
	switch status {
	case http.StatusUnauthorized:
		middleware.WriteUnauthorized(w)
	case http.StatusForbidden:
		if outcome == outcomeCollectorLimit {
			middleware.WriteCollectorLimitReached(w)
			return
		}
		middleware.WriteForbidden(w)
	case http.StatusRequestEntityTooLarge:
		middleware.WriteTooLarge(w)
	case http.StatusBadRequest:
		middleware.WriteBadRequest(w)
	case http.StatusInternalServerError:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "Internal Server Error\n")
	default:
		middleware.WriteUnavailable(w)
	}
}

// sqlState is the SQLSTATE of the Postgres error err wraps, or "".
func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// setOutcome stamps opamp.outcome on the request's server span: what the
// poll did, never an error's text, a value or a name.
func setOutcome(ctx context.Context, outcome string) {
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("opamp.outcome", outcome))
}
