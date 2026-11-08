package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"

	"github.com/telflo/opamp-server/store/collectors"
	"github.com/telflo/opamp-server/store/enrollmenttokens"
	"github.com/telflo/opamp-server/store/orgvariables"
	"github.com/telflo/opamp-server/tokens"
)

// The two polls: a default token's hand-off, and a rotate token's
// configuration. Every read comes first (the collector, its configuration,
// the rotate token to push), then the writes (the error row, the one
// Report): a poll that fails answers 503 before it records a delivery it
// never sent.

// handOff answers a default token's poll: the collector is enrolled (or
// found), its reports recorded, and it is pushed the fleet's current rotate
// token in its connection settings, with ReportFullState, so its next poll
// comes on that token and is served there. A default token is served no
// configuration. A fleet without a valid rotate token is suspended:
// acknowledged, pushed nothing, served nothing. Without a push (suspended,
// or an agent that takes no connection settings) the full-state ask is
// spaced like an unassigned collector's, at most once every
// FullStateInterval per collector.
func (h *AgentHandler) handOff(ctx context.Context, q *poll) (*protobufs.ServerToAgent, string, error) {
	found, err := h.collector(ctx, q)
	if errors.Is(err, errDescriptionNeeded) {
		return acknowledge(q, true), outcomeDescriptionAsked, nil
	}
	if err != nil {
		return nil, "", err
	}
	c := found.Collector
	answer := acknowledge(q, false)
	report := collectors.Report{}
	outcome := outcomeHandOffNoSettings
	pushed := false
	if acceptsSettings(q.msg) {
		if pushed, err = h.pushRotateToken(ctx, q, answer, c.Name); err != nil {
			return nil, "", err
		}
		outcome = outcomeHandOff
		if !pushed {
			outcome = outcomeSuspended
		}
	}
	switch {
	case pushed:
		answer.Flags |= uint64(protobufs.ServerToAgentFlags_ServerToAgentFlags_ReportFullState)
	case fullStateDue(c, q.now):
		answer.Flags |= uint64(protobufs.ServerToAgentFlags_ServerToAgentFlags_ReportFullState)
		report.FullStateRequestedAt = &q.now
	}
	if err := h.write(ctx, q, c, observe(q, c), report); err != nil {
		return nil, "", err
	}
	return answer, outcome, nil
}

// serve answers a rotate token's poll: what resolves for the collector (its
// pin, else its group's assigned published version), filled in with its
// organization variables, is pushed when its rendered SHA-256 differs from
// what was served, unless the collector already runs those bytes; nothing
// resolved, it is sent no configuration and asked for its full state at
// most once every FullStateInterval. A token in its grace (do_refresh) is
// pushed the fleet's current rotate token as well.
//
// The served-variables marker is the newest stamp the text was filled in
// with (orgvariables.WantedStamp): written with a push or a reconcile, and
// alone when the bytes served are already the resolved version's filled-in
// ones but the marker isn't that stamp. A filled-in text over
// orgvariables.MaxRenderedBytes is never built (serveTooLarge).
func (h *AgentHandler) serve(ctx context.Context, q *poll) (*protobufs.ServerToAgent, string, error) {
	found, err := h.collector(ctx, q)
	if errors.Is(err, errDescriptionNeeded) {
		return acknowledge(q, true), outcomeDescriptionAsked, nil
	}
	if err != nil {
		return nil, "", err
	}
	c := found.Collector
	o := observe(q, c)
	answer := acknowledge(q, false)

	if q.p.DoRefresh && acceptsSettings(q.msg) {
		if _, err := h.pushRotateToken(ctx, q, answer, c.Name); err != nil {
			return nil, "", err
		}
	}

	if found.Resolution.From == collectors.FromNone {
		return h.serveUnassigned(ctx, q, c, o, answer)
	}

	yaml, err := h.configuration(ctx, q, found.Resolution)
	if err != nil {
		return nil, "", err
	}
	resolved, err := h.resolve(ctx, q, found, c)
	if err != nil {
		return nil, "", err
	}
	// The length first: a text over the cap is never built. The names come
	// from the fill-in, never from the names stored at publish, so a version
	// published before migration 00012 is filled in too.
	n, names := orgvariables.RenderedLength(yaml, resolved.Values)
	wanted := orgvariables.WantedStamp(names, resolved, found.Resolution.GroupBindingStamp, stampOf(c.PinnedVariableGroupStamp))
	// The marker never covers a change the poll statement didn't see: a
	// cache miss reads the values at a revision past the one the statement
	// read the collector's bindings and stamps at, so a rebinding and an
	// override landing in between could otherwise mark the collector as
	// served a change whose values it never got. Capped, it reads pending
	// until its next poll.
	wanted = min(wanted, found.VariablesRevision)
	if n > orgvariables.MaxRenderedBytes {
		return h.serveTooLarge(ctx, q, c, o, answer, yaml, resolved)
	}
	rendered, _ := orgvariables.Render(yaml, resolved.Values)
	report := collectors.Report{}
	if rendered == "" {
		// An empty configuration is never pushed; nothing is recorded
		// served.
		if err := h.write(ctx, q, c, o, report); err != nil {
			return nil, "", err
		}
		return answer, outcomeEmptyYAML, nil
	}
	sum := sha256.Sum256([]byte(rendered))
	renderedSHA := hex.EncodeToString(sum[:])
	ref := found.Resolution.Ref()
	running := reportedHash(o, c)

	// Asking for the full state: a collector that has polled before (its
	// sequence past 1) whose report is missing, or stuck applying or failed
	// past StuckThreshold; a stuck failed one is re-delivered too, since a
	// bare ask only makes it repeat its verdict. Both at most once every
	// FullStateInterval per collector, whichever replica asks
	// (full_state_requested_at).
	due := q.msg.GetSequenceNum() > 1 && fullStateDue(c, q.now)
	stuckFailed := stuck(c, collectors.StatusFailed, q.now)
	if due && (c.EffectiveSHA256 == nil || stuck(c, collectors.StatusApplying, q.now) || stuckFailed) {
		answer.Flags |= uint64(protobufs.ServerToAgentFlags_ServerToAgentFlags_ReportFullState)
		report.FullStateRequestedAt = &q.now
	}
	redeliver := due && stuckFailed

	outcome := outcomeAck
	switch {
	case c.ServedSHA256 == nil || *c.ServedSHA256 != renderedSHA || redeliver:
		if !redeliver && running == renderedSHA {
			// The collector already runs these bytes (a row deleted and
			// re-enrolled, or a retarget to identical bytes): move the
			// served pointer without a delivery.
			report.Reconciled = &collectors.Reconciled{Ref: *ref, SHA256: renderedSHA, At: reconcileAt(q, o, c), VariablesStamp: wanted}
			outcome = outcomeReconciled
			break
		}
		if !acceptsRemoteConfig(q.msg) {
			// An agent that doesn't take a configuration is pushed none,
			// and nothing is recorded served.
			outcome = outcomeNotAccepted
			break
		}
		// A push, recorded as served now. A re-delivery is a fresh push of
		// the same bytes, recorded as one, so the collector's next report
		// is read against it.
		answer.RemoteConfig = remoteConfig(rendered, sum[:])
		report.Served = &collectors.Served{Ref: *ref, SHA256: renderedSHA, At: q.now, VariablesStamp: wanted}
		outcome = outcomePushed
		if redeliver {
			outcome = outcomeRedelivered
		}
	case running == renderedSHA && (c.Served == nil || *c.Served != *ref):
		// Served these bytes already, under another published version (an
		// identical copy retargeted): the pointer follows, the served time
		// stays.
		report.Reconciled = &collectors.Reconciled{Ref: *ref, SHA256: renderedSHA, At: reconcileAt(q, o, c), VariablesStamp: wanted}
		outcome = outcomeReconciled
	}
	if report.Served == nil && report.Reconciled == nil && c.Served != nil && *c.Served == *ref &&
		c.ServedSHA256 != nil && *c.ServedSHA256 == renderedSHA && stampOf(c.ServedVariablesStamp) != wanted {
		// Served these bytes of the resolved version already, but before a
		// change it uses that left the bytes as they were (an override
		// equal to the value it hides, a binding to a group that overrides
		// nothing it uses): the marker moves without a push.
		report.VariablesStamp = &wanted
	}
	if err := h.write(ctx, q, c, o, report); err != nil {
		return nil, "", err
	}
	return answer, outcome, nil
}

// tooLargeMessage is the error row of a configuration whose filled-in text
// is over the cap: fixed, naming no value.
const tooLargeMessage = "The configuration with its organization variables filled in is over 1 MiB."

// serveTooLarge answers a poll whose resolved configuration, filled in with
// resolved, is over orgvariables.MaxRenderedBytes: the text is never built.
// Its SHA-256 is streamed through RenderTo, and the collector, which keeps
// running what it has, gets one error row of that hash with the fixed
// message (OncePerHash: a later poll of the same text adds none). Nothing
// is pushed or recorded served, so it reads pending; its reports are
// written as on any poll.
func (h *AgentHandler) serveTooLarge(ctx context.Context, q *poll, c collectors.Collector, o observed,
	answer *protobufs.ServerToAgent, yaml string, resolved orgvariables.Resolved) (*protobufs.ServerToAgent, string, error) {
	hash := sha256.New()
	if err := orgvariables.RenderTo(hash, yaml, resolved.Values); err != nil {
		return nil, "", err // a hash's writes never fail
	}
	sha := hex.EncodeToString(hash.Sum(nil))
	err := h.collectors.RecordError(ctx, q.p.OrganizationID, q.p.FleetID, c.ID, collectors.ErrorReport{
		ConfigSHA256: &sha, Message: tooLargeMessage, At: q.now, OncePerHash: true})
	if err != nil && !errors.Is(err, collectors.ErrNotFound) {
		return nil, "", err
	}
	if err := h.write(ctx, q, c, o, collectors.Report{}); err != nil {
		return nil, "", err
	}
	return answer, outcomeRenderedTooLarge, nil
}

// resolve is what the collector's text is filled in with: nothing, read
// from nowhere, for an organization whose counter never moved (it never had
// a variable); else the variables cache's entry for the organization's
// generation and counter, as Poll read them, and the collector's two
// variable groups (its group's, which applies under a pin too, and its
// pin's), or on a miss the store's answer, kept under that generation and
// the revision it was read at. A store failure is the poll's 503.
func (h *AgentHandler) resolve(ctx context.Context, q *poll, found collectors.PollResult, c collectors.Collector) (orgvariables.Resolved, error) {
	if found.VariablesRevision == 0 {
		return orgvariables.Resolved{}, nil
	}
	group, pinned := found.Resolution.GroupVariableGroupID, c.PinnedVariableGroupID
	generation := found.OrganizationGeneration
	if r, ok := h.vars.Get(q.p.OrganizationID, generation, found.VariablesRevision, group, pinned); ok {
		return r, nil
	}
	r, err := h.variables.Variables(ctx, q.p.OrganizationID, group, pinned)
	if err != nil {
		return orgvariables.Resolved{}, err
	}
	h.vars.Put(q.p.OrganizationID, generation, group, pinned, r)
	return r, nil
}

// stampOf is a nullable stamp, 0 for none.
func stampOf(stamp *int64) int64 {
	if stamp == nil {
		return 0
	}
	return *stamp
}

// serveUnassigned answers a rotate token's poll for a collector nothing
// resolves for (no pin, and no published version assigned to its group): it
// keeps running what it has, its own file or the last published version it
// was sent, and is sent no configuration (in a grace, `serve` has pushed it
// the rotate token already). Its reports are recorded, and it is asked for
// its full state at most once every FullStateInterval, whatever its fleet's
// type (so a collector that never sends its configuration isn't asked on
// every poll), else acknowledged.
func (h *AgentHandler) serveUnassigned(ctx context.Context, q *poll, c collectors.Collector, o observed,
	answer *protobufs.ServerToAgent) (*protobufs.ServerToAgent, string, error) {
	report := collectors.Report{}
	outcome := outcomeAck
	if fullStateDue(c, q.now) {
		answer.Flags |= uint64(protobufs.ServerToAgentFlags_ServerToAgentFlags_ReportFullState)
		report.FullStateRequestedAt = &q.now
		outcome = outcomeFullState
	}
	if err := h.write(ctx, q, c, o, report); err != nil {
		return nil, "", err
	}
	return answer, outcome, nil
}

// collector finds the poll's collector by Poll (one statement: the row
// before its heartbeat, the heartbeat, and what resolves), enrolling it
// first when the organization holds none of its instance uid. A collector
// enrolled in another fleet than the token's is refused, as Poll wrote
// nothing for it. One the organization doesn't hold is enrolled only by a
// message carrying its agent description, else errDescriptionNeeded: the
// description names the group it asks for, and enrolling it into the
// Default group without one would make a refusal of a foreign group last
// only until the supervisor sent a message without its description. A
// description naming no group is refused (errNoGroup); a collector already
// enrolled is never checked again.
func (h *AgentHandler) collector(ctx context.Context, q *poll) (collectors.PollResult, error) {
	request := collectors.PollRequest{
		OrganizationID: q.p.OrganizationID, FleetID: q.p.FleetID, InstanceUID: q.uid,
		TokenID: q.p.TokenID, At: q.now,
	}
	found, err := h.collectors.Poll(ctx, request)
	if err != nil {
		return collectors.PollResult{}, err
	}
	if !found.Found {
		if q.msg.GetAgentDescription() == nil {
			return collectors.PollResult{}, errDescriptionNeeded
		}
		group, err := reportedGroup(q.msg)
		if err != nil {
			return collectors.PollResult{}, err
		}
		if _, err := h.collectors.Enroll(ctx, collectors.Enrollment{
			OrganizationID: q.p.OrganizationID, FleetID: q.p.FleetID, InstanceUID: q.uid,
			TokenID: q.p.TokenID, DesiredName: q.serverName, GroupID: &group, At: q.now,
		}); err != nil {
			return collectors.PollResult{}, err
		}
		if found, err = h.collectors.Poll(ctx, request); err != nil {
			return collectors.PollResult{}, err
		}
		if !found.Found {
			return collectors.PollResult{}, errCollectorVanished
		}
	}
	if found.Collector.FleetID != q.p.FleetID {
		return collectors.PollResult{}, errFleetMismatch
	}
	return found, nil
}

// configuration is what resolved's YAML is: from the cache, kept by
// organization and SHA-256, else read from the store for the organization,
// its SHA-256 checked against the one resolved, and cached. Deleted between
// the poll and the read: errConfigurationGone, the next poll resolves
// again.
func (h *AgentHandler) configuration(ctx context.Context, q *poll, resolved collectors.Resolution) (string, error) {
	if yaml, ok := h.cache.Get(q.p.OrganizationID, resolved.SHA256); ok {
		return yaml, nil
	}
	read, err := h.collectors.YAML(ctx, q.p.OrganizationID, *resolved.Ref())
	switch {
	case errors.Is(err, collectors.ErrNotFound):
		return "", errConfigurationGone
	case err != nil:
		return "", err
	}
	sum := sha256.Sum256([]byte(read.YAML))
	if hex.EncodeToString(sum[:]) != resolved.SHA256 {
		return "", errHashMismatch
	}
	h.cache.Put(q.p.OrganizationID, resolved.SHA256, read.YAML)
	return read.YAML, nil
}

// pushRotateToken puts the fleet's current rotate token in the answer's
// connection settings: a fresh bearer for it, minted under the current key
// with the token's row's fleet, the destination endpoint
// (OPAMP_SERVER_URL), the heartbeat interval (the OpAMP client refuses an
// offer without either), and the collector's name as ServerName, since the
// supervisor replaces its headers with the offer's. It reports false,
// pushing nothing, when the fleet has no valid rotate token (suspended) or
// the current one is the poll's own.
func (h *AgentHandler) pushRotateToken(ctx context.Context, q *poll, answer *protobufs.ServerToAgent, name string) (bool, error) {
	current, err := h.tokens.GetActiveRotate(ctx, q.p.OrganizationID, q.p.FleetID, q.now)
	switch {
	case errors.Is(err, enrollmenttokens.ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	case current.ID == q.p.TokenID:
		return false, nil
	}
	bearer, err := h.keyring.Mint(tokens.Claims{TokenID: current.ID, FleetID: current.FleetID})
	if err != nil {
		return false, err
	}
	headers := []*protobufs.Header{{Key: "Authorization", Value: "Bearer " + bearer}}
	// Only a name an HTTP header can carry (collectors.ValidName refuses
	// every control character): the supervisor sends it on every request,
	// and one it can't send breaks them all.
	if collectors.ValidName(name) {
		headers = append(headers, &protobufs.Header{Key: "ServerName", Value: name})
	}
	answer.ConnectionSettings = &protobufs.ConnectionSettingsOffers{
		Opamp: &protobufs.OpAMPConnectionSettings{
			DestinationEndpoint:      h.serverURL,
			Headers:                  &protobufs.Headers{Headers: headers},
			HeartbeatIntervalSeconds: uint64(h.heartbeat.Seconds()),
		},
	}
	return true, nil
}

// write records what the poll learned: the error row of a failure, then the
// one Report, which writes nothing when every part is as stored, and isn't
// called when the poll has no part to write. The error row goes first: a
// Report failing after it is retried, and a row twice beats a row lost.
func (h *AgentHandler) write(ctx context.Context, q *poll, c collectors.Collector, o observed, report collectors.Report) error {
	if o.failure != nil {
		err := h.collectors.RecordError(ctx, q.p.OrganizationID, q.p.FleetID, c.ID, *o.failure)
		if err != nil && !errors.Is(err, collectors.ErrNotFound) {
			return err
		}
	}
	report.OrganizationID, report.FleetID, report.CollectorID = q.p.OrganizationID, q.p.FleetID, c.ID
	report.Reported, report.Effective = o.reported, o.effective
	if report.Served == nil && report.Reconciled == nil && report.Effective == nil && report.VariablesStamp == nil &&
		report.FullStateRequestedAt == nil && reportedEmpty(report.Reported) {
		return nil
	}
	_, err := h.collectors.Report(ctx, report)
	if errors.Is(err, collectors.ErrPublicationNotFound) {
		return errConfigurationGone
	}
	return err
}

func reportedEmpty(r collectors.Reported) bool {
	return r.CollectorVersion == nil && r.IP == nil && r.HostIP == nil && r.Attributes == nil
}

// reconcileAt is when a reconciled collector counts as served if it never
// was: the earlier of now and its report's time, so its report reads as one
// of the served bytes.
func reconcileAt(q *poll, o observed, c collectors.Collector) time.Time {
	if o.effective == nil && c.EffectiveAt != nil && c.EffectiveAt.Before(q.now) {
		return *c.EffectiveAt
	}
	return q.now
}

// acknowledge is an answer to the poll, carrying the collector's instance
// uid as its raw 16 bytes (the OpAMP specification's), asking for its full
// state when fullState.
func acknowledge(q *poll, fullState bool) *protobufs.ServerToAgent {
	answer := &protobufs.ServerToAgent{InstanceUid: append([]byte(nil), q.uid[:]...)}
	if fullState {
		answer.Flags = uint64(protobufs.ServerToAgentFlags_ServerToAgentFlags_ReportFullState)
	}
	return answer
}

// remoteConfig is a push of rendered: one file, "otelcol.yml", its hash the
// SHA-256 of its bytes.
func remoteConfig(rendered string, sum []byte) *protobufs.AgentRemoteConfig {
	return &protobufs.AgentRemoteConfig{
		Config: &protobufs.AgentConfigMap{ConfigMap: map[string]*protobufs.AgentConfigObject{
			"otelcol.yml": {Body: []byte(rendered), ContentType: "text/yaml"},
		}},
		ConfigHash: append([]byte(nil), sum...),
	}
}
