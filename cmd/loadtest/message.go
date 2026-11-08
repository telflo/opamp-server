package main

import (
	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"
)

// agentCapabilities is the mask each virtual supervisor advertises. It mirrors
// the capabilities a real OTel supervisor sends (see
// integration-tests/fixtures/supervisor/linux_server_arm64.yaml) and, crucially,
// includes AcceptsOpAMPConnectionSettings (bit 0x100) so the server performs its
// full rotation-attach work on every poll — the realistic per-request cost we
// want to measure.
const agentCapabilities = uint64(
	protobufs.AgentCapabilities_AgentCapabilities_ReportsStatus |
		protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig |
		protobufs.AgentCapabilities_AgentCapabilities_ReportsEffectiveConfig |
		protobufs.AgentCapabilities_AgentCapabilities_ReportsHealth |
		protobufs.AgentCapabilities_AgentCapabilities_ReportsRemoteConfig |
		protobufs.AgentCapabilities_AgentCapabilities_AcceptsOpAMPConnectionSettings,
)

// serverToAgentFlagReportFullState mirrors the server's ReportFullState flag
// (protobufs.ServerToAgentFlags_ServerToAgentFlags_ReportFullState, bit 0). When the server sets
// it, the agent is expected to resend its full state on the next poll.
const serverToAgentFlagReportFullState uint64 = 1 << 0

// reportedCollectorVersion is the service.version each fake collector claims to
// be running: a recent real release, so the fleet looks like real collectors.
const reportedCollectorVersion = "0.162.0"

// reportedOSType is the os.type each fake collector reports. The server-fleet
// path doesn't persist it, but a real supervisor always sends it, so we include
// it for fidelity.
const reportedOSType = "linux"

// noopEffectiveConfig is the tiny valid-looking collector config a virtual
// supervisor reports as its effective state before the server has delivered a
// remote config. Content is irrelevant to load — the server only hashes and
// stores it — so we keep it minimal.
const noopEffectiveConfig = "receivers: {}\nexporters: {}\nservice:\n  pipelines: {}\n"

// identity is the stable per-instance descriptor a virtual supervisor reports in
// its AgentDescription. Distinct host.ip/host.name per client make the fleet's
// collector list show N genuinely different machines; groupID (telflo.collector_group_id)
// binds the instance to a collector group so the server resolves a config to
// deliver. main requires it (the server refuses a collector naming no
// group); an empty one, which only a test passes, is left out.
type identity struct {
	instanceUID [16]byte
	hostName    string
	hostIP      string
	groupID     string
}

// pollMessage is everything one poll needs to build its AgentToServer: the
// stable identity, the sequence/full-state bookkeeping, and the two pieces of
// mutable per-instance state the server cares about — the config currently held
// in memory (reported as EffectiveConfig on full-state polls) and the hash of
// the last remote config the server delivered (reported as RemoteConfigStatus
// once it exists).
type pollMessage struct {
	id               identity
	seq              uint64
	fullState        bool
	effectiveConfig  []byte // reported as EffectiveConfig on full-state polls
	remoteConfigHash []byte // reported as RemoteConfigStatus(APPLIED) when non-nil
}

// buildAgentToServer marshals the AgentToServer protobuf for one poll.
//
// Every poll carries the identity fields the server requires: the 16-byte
// InstanceUid, the monotonically increasing SequenceNum, and the capability
// mask. When fullState is true (the first poll, or after the server returns the
// ReportFullState flag) it also attaches AgentDescription, EffectiveConfig, and
// Health — matching how a real supervisor economizes bytes between full dumps.
// Once the server has delivered a remote config, every poll also reports
// RemoteConfigStatus{APPLIED, hash} so the server flips the instance to in-sync.
func buildAgentToServer(p pollMessage) ([]byte, error) {
	msg := &protobufs.AgentToServer{
		InstanceUid:  p.id.instanceUID[:],
		SequenceNum:  p.seq,
		Capabilities: agentCapabilities,
	}
	if p.fullState {
		msg.AgentDescription = buildAgentDescription(p.id)
		msg.EffectiveConfig = buildEffectiveConfig(p.effectiveConfig)
		msg.Health = &protobufs.ComponentHealth{Healthy: true, Status: "ok"}
	}
	if len(p.remoteConfigHash) > 0 {
		msg.RemoteConfigStatus = &protobufs.RemoteConfigStatus{
			LastRemoteConfigHash: p.remoteConfigHash,
			Status:               protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED,
		}
	}
	return proto.Marshal(msg)
}

// buildAgentDescription assembles the per-instance descriptor, placing each
// attribute in the bucket the server reads it from: service.version is
// identifying; host.ip, host.name, os.type and telflo.collector_group_id are
// non-identifying. The group id is omitted when empty (main never passes
// one): a blank value would be refused just as a missing one is.
func buildAgentDescription(id identity) *protobufs.AgentDescription {
	nonIdentifying := []*protobufs.KeyValue{
		stringAttr("host.ip", id.hostIP),
		stringAttr("host.name", id.hostName),
		stringAttr("os.type", reportedOSType),
	}
	if id.groupID != "" {
		nonIdentifying = append(nonIdentifying, stringAttr("telflo.collector_group_id", id.groupID))
	}
	return &protobufs.AgentDescription{
		IdentifyingAttributes: []*protobufs.KeyValue{
			stringAttr("service.version", reportedCollectorVersion),
		},
		NonIdentifyingAttributes: nonIdentifying,
	}
}

// buildEffectiveConfig wraps the in-memory config body in the single-file config
// map a supervisor reports. An empty body falls back to the noop config so the
// very first full-state poll still reports something plausible.
func buildEffectiveConfig(body []byte) *protobufs.EffectiveConfig {
	if len(body) == 0 {
		body = []byte(noopEffectiveConfig)
	}
	return &protobufs.EffectiveConfig{
		ConfigMap: &protobufs.AgentConfigMap{
			ConfigMap: map[string]*protobufs.AgentConfigObject{
				"otelcol.yml": {
					Body:        body,
					ContentType: "text/yaml",
				},
			},
		},
	}
}

func stringAttr(key, value string) *protobufs.KeyValue {
	return &protobufs.KeyValue{
		Key: key,
		Value: &protobufs.AnyValue{
			Value: &protobufs.AnyValue_StringValue{StringValue: value},
		},
	}
}

// serverResponse is the subset of a ServerToAgent the poll loop acts on: whether
// to resend full state next poll, the rotated bearer token to switch to (empty
// when none offered), and any delivered remote config (body + hash) to store in
// memory.
type serverResponse struct {
	reportFullState  bool
	newToken         string
	remoteConfigBody []byte
	remoteConfigHash []byte
}

// parseServerToAgent decodes a ServerToAgent response into the actions the poll
// loop takes: honor the ReportFullState flag, adopt a rotated token from the
// OpAMP connection-settings offer, and store any delivered remote config. Unlike
// a real supervisor it performs no work to "apply" the config — it just holds
// the bytes and echoes the hash back — but from the server's perspective the
// exchange is identical.
func parseServerToAgent(body []byte) (serverResponse, error) {
	var msg protobufs.ServerToAgent
	if err := proto.Unmarshal(body, &msg); err != nil {
		return serverResponse{}, err
	}
	resp := serverResponse{
		reportFullState: msg.Flags&serverToAgentFlagReportFullState != 0,
		newToken:        rotatedToken(&msg),
	}
	if rc := msg.GetRemoteConfig(); rc != nil {
		resp.remoteConfigHash = rc.GetConfigHash()
		resp.remoteConfigBody = firstConfigBody(rc)
	}
	return resp, nil
}

// rotatedToken pulls the Authorization header out of an OpAMP connection-settings
// offer — the per-instance token the supervisor must switch to. Returns "" when
// the response carries no offer (or no Authorization header on it).
func rotatedToken(msg *protobufs.ServerToAgent) string {
	for _, h := range msg.GetConnectionSettings().GetOpamp().GetHeaders().GetHeaders() {
		if h.GetKey() == "Authorization" {
			return h.GetValue()
		}
	}
	return ""
}

// firstConfigBody returns the body of the (single) config file the server
// delivers. Telflo always keys the remote config under one file, so the first
// entry is the whole config.
func firstConfigBody(rc *protobufs.AgentRemoteConfig) []byte {
	for _, f := range rc.GetConfig().GetConfigMap() {
		return f.GetBody()
	}
	return nil
}

// newInstanceUID returns a fresh random 16-byte instance id. uuid.New panics
// only if the OS entropy source fails, which is fatal for a load run anyway.
func newInstanceUID() [16]byte {
	return uuid.New()
}
