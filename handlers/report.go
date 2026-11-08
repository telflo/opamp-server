package handlers

import (
	"encoding/hex"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/protobufs"

	"github.com/telflo/opamp-server/store/collectors"
	"github.com/telflo/opamp-server/store/fleets"
)

// What a poll reads from the message, for its one Report.

// The agent's capability bit 0x100, AcceptsOpAMPConnectionSettings: only an
// agent that set it is pushed connection settings. And 0x2,
// AcceptsRemoteConfig: only an agent that set it is pushed a configuration
// (the supervisor's accepts_remote_config).
const (
	acceptsConnectionSettings = uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsOpAMPConnectionSettings)
	acceptsRemoteConfigBit    = uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig)
)

// The description's attributes the poll reads.
const (
	attributeServiceVersion = "service.version"
	attributeHostIP         = "host.ip"
	attributeGroupID        = "telflo.collector_group_id"
)

// StuckThreshold is how long a collector may report applying, or failed,
// before it is asked for its full state (and a failed one re-delivered).
// FullStateInterval spaces those asks, and an unassigned collector's, per
// collector across replicas (full_state_requested_at).
const (
	StuckThreshold    = 3 * time.Minute
	FullStateInterval = 3 * time.Minute
)

// emptyFailureMessage is what a FAILED report without a message records:
// an error row needs a message.
const emptyFailureMessage = "The collector reported a failure without a message."

// maxStoredAttributes bounds how many of a description's attributes are
// considered for storing (by name), before the store's byte bound.
const maxStoredAttributes = 256

// observed is what one message says of its collector, as the parts of the
// poll's Report and its error row.
type observed struct {
	reported  collectors.Reported
	effective *collectors.Effective
	failure   *collectors.ErrorReport
}

// observe reads the message's reported fields (each only when it differs
// from the stored one, or, for the attributes, whenever the message
// carries a description) and its RemoteConfigStatus.
func observe(q *poll, c collectors.Collector) observed {
	var o observed
	if desc := q.msg.GetAgentDescription(); desc != nil {
		if v := stringAttribute(desc.GetIdentifyingAttributes(), attributeServiceVersion); v != "" &&
			fleets.ValidText(v, 1, collectors.MaxCollectorVersionLength) && !equal(c.ReportedCollectorVersion, v) {
			o.reported.CollectorVersion = &v
		}
		if ip := hostIP(desc); ip != "" && fleets.ValidText(ip, 1, collectors.MaxAddressLength) && !equal(c.ReportedHostIP, ip) {
			o.reported.HostIP = &ip
		}
		o.reported.Attributes = boundedAttributes(descriptionAttributes(desc))
	}
	if ip := strings.TrimSpace(q.p.ClientIP); ip != "" && fleets.ValidText(ip, 1, collectors.MaxAddressLength) &&
		!equal(c.ReportedIP, ip) {
		o.reported.IP = &ip
	}
	o.effective, o.failure = effectiveOf(q.msg, q.now)
	return o
}

// effectiveOf is the message's RemoteConfigStatus as the effective part
// (the hash the collector reports, never its effective configuration's
// bytes) and, for a failure, its error row. UNSET (the protobuf zero) and a
// hash that isn't a SHA-256 record nothing.
//
// The health gate: the supervisor answers a re-delivered configuration it
// already runs with APPLIED at once, even while the collector crash-loops
// on it, and reports the crash in the same message's Health. An APPLIED
// with an unhealthy Health carrying a last_error is recorded as failed,
// with that error. An unhealthy Health without one is the supervisor's
// report before it starts the collector, not a crash.
func effectiveOf(msg *protobufs.AgentToServer, now time.Time) (*collectors.Effective, *collectors.ErrorReport) {
	rcs := msg.GetRemoteConfigStatus()
	if rcs == nil || len(rcs.GetLastRemoteConfigHash()) != 32 {
		return nil, nil
	}
	status, ok := mapRemoteConfigStatus(rcs.GetStatus())
	if !ok {
		return nil, nil
	}
	message := rcs.GetErrorMessage()
	if status == collectors.StatusApplied {
		if health := msg.GetHealth(); health != nil && !health.GetHealthy() && health.GetLastError() != "" {
			status, message = collectors.StatusFailed, health.GetLastError()
		}
	}
	sha := hex.EncodeToString(rcs.GetLastRemoteConfigHash())
	e := &collectors.Effective{SHA256: sha, Status: status, At: now}
	if status != collectors.StatusFailed {
		return e, nil
	}
	if strings.TrimSpace(message) == "" {
		message = emptyFailureMessage
	}
	e.Error = message
	return e, &collectors.ErrorReport{ConfigSHA256: &sha, Message: message, At: now}
}

// mapRemoteConfigStatus maps the OpAMP status to the status stored, and
// reports false for UNSET or a value this build doesn't know (nothing to
// record).
func mapRemoteConfigStatus(s protobufs.RemoteConfigStatuses) (collectors.EffectiveStatus, bool) {
	switch s {
	case protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED:
		return collectors.StatusApplied, true
	case protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLYING:
		return collectors.StatusApplying, true
	case protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED:
		return collectors.StatusFailed, true
	}
	return "", false
}

// reportedHash is the hash the collector says it runs: this message's
// RemoteConfigStatus, else the stored one; "" for neither.
func reportedHash(o observed, c collectors.Collector) string {
	if o.effective != nil {
		return o.effective.SHA256
	}
	if c.EffectiveSHA256 != nil {
		return *c.EffectiveSHA256
	}
	return ""
}

// stuck reports whether c has reported status for longer than
// StuckThreshold at now (as stored before this poll).
func stuck(c collectors.Collector, status collectors.EffectiveStatus, now time.Time) bool {
	return c.EffectiveStatus != nil && *c.EffectiveStatus == status &&
		c.EffectiveAt != nil && now.Sub(*c.EffectiveAt) > StuckThreshold
}

// fullStateDue reports whether c may be asked for its full state at now:
// never asked, or last asked FullStateInterval ago or more, by any replica.
func fullStateDue(c collectors.Collector, now time.Time) bool {
	return c.FullStateRequestedAt == nil || !now.Before(c.FullStateRequestedAt.Add(FullStateInterval))
}

// acceptsSettings reports whether the agent applies pushed connection
// settings (capability 0x100).
func acceptsSettings(msg *protobufs.AgentToServer) bool {
	return msg.GetCapabilities()&acceptsConnectionSettings != 0
}

// acceptsRemoteConfig reports whether the agent applies a pushed
// configuration (capability 0x2).
func acceptsRemoteConfig(msg *protobufs.AgentToServer) bool {
	return msg.GetCapabilities()&acceptsRemoteConfigBit != 0
}

// trimmedServerName is the ServerName header the supervisor sends, trimmed:
// the name a new collector asks for ("" leaves it to a petname).
func trimmedServerName(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get("ServerName"))
}

// reportedGroup is the group a new collector's description names
// (telflo.collector_group_id). A description naming none (absent or blank)
// is errNoGroup: every collector names its group, never enrolled into the
// Default group; a value that isn't a group's id is errForeignGroup. The
// caller asks for the full state first when the message carries no
// description at all.
func reportedGroup(msg *protobufs.AgentToServer) (uuid.UUID, error) {
	raw := strings.TrimSpace(stringAttribute(msg.GetAgentDescription().GetNonIdentifyingAttributes(), attributeGroupID))
	if raw == "" {
		return uuid.Nil, errNoGroup
	}
	id, err := uuid.Parse(raw)
	if err != nil || id == uuid.Nil {
		return uuid.Nil, errForeignGroup
	}
	return id, nil
}

// stringAttribute is the string value of key among attributes, trimmed, or
// "".
func stringAttribute(attributes []*protobufs.KeyValue, key string) string {
	for _, kv := range attributes {
		if kv.GetKey() == key {
			return strings.TrimSpace(kv.GetValue().GetStringValue())
		}
	}
	return ""
}

// hostIP is the machine's own address the description reports (host.ip,
// a non-identifying attribute the launch path sets): a string, or the
// first non-empty string of an array (the semantic conventions' shape).
func hostIP(desc *protobufs.AgentDescription) string {
	for _, kv := range desc.GetNonIdentifyingAttributes() {
		if kv.GetKey() != attributeHostIP {
			continue
		}
		v := kv.GetValue()
		if s := strings.TrimSpace(v.GetStringValue()); s != "" {
			return s
		}
		for _, element := range v.GetArrayValue().GetValues() {
			if s := strings.TrimSpace(element.GetStringValue()); s != "" {
				return s
			}
		}
		return ""
	}
	return ""
}

// descriptionAttributes is the description's attributes, identifying and
// non-identifying, each value as text (a string, an integer, a boolean or
// a double; arrays, maps and bytes are left out); an identifying one wins
// over a non-identifying one of the same name.
func descriptionAttributes(desc *protobufs.AgentDescription) map[string]string {
	out := map[string]string{}
	for _, list := range [][]*protobufs.KeyValue{desc.GetNonIdentifyingAttributes(), desc.GetIdentifyingAttributes()} {
		for _, kv := range list {
			if text, ok := anyValueText(kv.GetValue()); ok && kv.GetKey() != "" {
				out[kv.GetKey()] = text
			}
		}
	}
	return out
}

func anyValueText(v *protobufs.AnyValue) (string, bool) {
	switch value := v.GetValue().(type) {
	case *protobufs.AnyValue_StringValue:
		return value.StringValue, true
	case *protobufs.AnyValue_IntValue:
		return strconv.FormatInt(value.IntValue, 10), true
	case *protobufs.AnyValue_BoolValue:
		return strconv.FormatBool(value.BoolValue), true
	case *protobufs.AnyValue_DoubleValue:
		return strconv.FormatFloat(value.DoubleValue, 'g', -1, 64), true
	}
	return "", false
}

// boundedAttributes is attributes as the store takes them: the names in
// order, the first maxStoredAttributes considered, each kept while the
// whole stays within collectors.MaxAttributesBytes (collectors.
// ValidAttributes); a pair the store can't hold (invalid UTF-8, NUL) is
// left out.
func boundedAttributes(attributes map[string]string) map[string]string {
	out := make(map[string]string, len(attributes))
	if collectors.ValidAttributes(attributes) {
		for k, v := range attributes {
			out[k] = v
		}
		return out
	}
	names := make([]string, 0, len(attributes))
	for k := range attributes {
		names = append(names, k)
	}
	slices.Sort(names)
	if len(names) > maxStoredAttributes {
		names = names[:maxStoredAttributes]
	}
	for _, k := range names {
		out[k] = attributes[k]
		if !collectors.ValidAttributes(out) {
			delete(out, k)
		}
	}
	return out
}

func equal(stored *string, v string) bool {
	return stored != nil && *stored == v
}
