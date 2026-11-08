package main

import (
	"bytes"
	"testing"

	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"
)

func testIdentity() identity {
	return identity{
		instanceUID: newInstanceUID(),
		hostName:    "col-0001",
		hostIP:      "10.42.0.1",
		groupID:     "grp-123",
	}
}

// The server rejects any InstanceUid that is not exactly 16 bytes
// (uuid.FromBytes), and only does rotation-attach work when the agent advertises
// AcceptsOpAMPConnectionSettings (bit 0x100). Both are load-bearing for a
// faithful poll, so assert them on the marshaled request.
func TestBuildAgentToServer_RequiredFields(t *testing.T) {
	body, err := buildAgentToServer(pollMessage{id: testIdentity(), seq: 1, fullState: true})
	if err != nil {
		t.Fatalf("buildAgentToServer: %v", err)
	}

	var msg protobufs.AgentToServer
	if err := proto.Unmarshal(body, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(msg.InstanceUid) != 16 {
		t.Errorf("InstanceUid = %d bytes, want 16", len(msg.InstanceUid))
	}
	if msg.SequenceNum != 1 {
		t.Errorf("SequenceNum = %d, want 1", msg.SequenceNum)
	}
	if msg.Capabilities&0x100 == 0 {
		t.Errorf("capabilities 0x%x missing AcceptsOpAMPConnectionSettings (0x100)", msg.Capabilities)
	}
}

// A full-state poll must carry the per-instance identity the collector list
// returns: service.version (identifying) is its collectorVersion; host.ip,
// host.name, os.type and telflo.collector_group_id (non-identifying) are its
// hostIp, its attributes and its group. Assert each lands in the bucket the
// server reads it from.
func TestBuildAgentToServer_FullStateIdentity(t *testing.T) {
	id := testIdentity()
	body, err := buildAgentToServer(pollMessage{id: id, seq: 1, fullState: true})
	if err != nil {
		t.Fatalf("buildAgentToServer: %v", err)
	}
	var msg protobufs.AgentToServer
	if err := proto.Unmarshal(body, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got := identifyingAttr(&msg, "service.version"); got != reportedCollectorVersion {
		t.Errorf("service.version = %q, want %q", got, reportedCollectorVersion)
	}
	if got := nonIdentifyingAttr(&msg, "host.ip"); got != id.hostIP {
		t.Errorf("host.ip = %q, want %q", got, id.hostIP)
	}
	if got := nonIdentifyingAttr(&msg, "host.name"); got != id.hostName {
		t.Errorf("host.name = %q, want %q", got, id.hostName)
	}
	if got := nonIdentifyingAttr(&msg, "os.type"); got != reportedOSType {
		t.Errorf("os.type = %q, want %q", got, reportedOSType)
	}
	if got := nonIdentifyingAttr(&msg, "telflo.collector_group_id"); got != id.groupID {
		t.Errorf("telflo.collector_group_id = %q, want %q", got, id.groupID)
	}
}

// When no collector group is configured, the attribute must be omitted entirely
// rather than sent empty — an empty value would be a real association attempt
// the server would reject.
func TestBuildAgentToServer_GroupIDOmittedWhenEmpty(t *testing.T) {
	id := testIdentity()
	id.groupID = ""
	body, err := buildAgentToServer(pollMessage{id: id, seq: 1, fullState: true})
	if err != nil {
		t.Fatalf("buildAgentToServer: %v", err)
	}
	var msg protobufs.AgentToServer
	if err := proto.Unmarshal(body, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, kv := range msg.GetAgentDescription().GetNonIdentifyingAttributes() {
		if kv.GetKey() == "telflo.collector_group_id" {
			t.Errorf("telflo.collector_group_id should be omitted when groupID is empty")
		}
	}
}

// Full-state polls report the config held in memory as EffectiveConfig; compact
// polls omit the whole description/config block to save bytes.
func TestBuildAgentToServer_EffectiveConfig(t *testing.T) {
	id := testIdentity()
	cfg := []byte("receivers: {}\nexporters: {}\n")

	full, err := buildAgentToServer(pollMessage{id: id, seq: 1, fullState: true, effectiveConfig: cfg})
	if err != nil {
		t.Fatalf("full: %v", err)
	}
	var fullMsg protobufs.AgentToServer
	if err := proto.Unmarshal(full, &fullMsg); err != nil {
		t.Fatalf("unmarshal full: %v", err)
	}
	if got := effectiveConfigBody(&fullMsg); !bytes.Equal(got, cfg) {
		t.Errorf("EffectiveConfig body = %q, want %q", got, cfg)
	}

	compact, err := buildAgentToServer(pollMessage{id: id, seq: 2, fullState: false, effectiveConfig: cfg})
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	var compactMsg protobufs.AgentToServer
	if err := proto.Unmarshal(compact, &compactMsg); err != nil {
		t.Fatalf("unmarshal compact: %v", err)
	}
	if compactMsg.AgentDescription != nil || compactMsg.EffectiveConfig != nil {
		t.Errorf("compact poll should omit description/config")
	}
}

// Once a remote config has been applied, every poll (full or compact) reports
// RemoteConfigStatus{APPLIED, hash} so the server flips the instance to in-sync.
// Before the first config, no status is sent.
func TestBuildAgentToServer_RemoteConfigStatus(t *testing.T) {
	id := testIdentity()
	hash := []byte{0xde, 0xad, 0xbe, 0xef}

	applied, err := buildAgentToServer(pollMessage{id: id, seq: 3, fullState: false, remoteConfigHash: hash})
	if err != nil {
		t.Fatalf("applied: %v", err)
	}
	var msg protobufs.AgentToServer
	if err := proto.Unmarshal(applied, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	rcs := msg.GetRemoteConfigStatus()
	if rcs == nil {
		t.Fatal("expected RemoteConfigStatus when a config hash is held")
	}
	if !bytes.Equal(rcs.GetLastRemoteConfigHash(), hash) {
		t.Errorf("LastRemoteConfigHash = %x, want %x", rcs.GetLastRemoteConfigHash(), hash)
	}
	if rcs.GetStatus() != protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED {
		t.Errorf("Status = %v, want APPLIED", rcs.GetStatus())
	}

	none, err := buildAgentToServer(pollMessage{id: id, seq: 1, fullState: true})
	if err != nil {
		t.Fatalf("none: %v", err)
	}
	var noneMsg protobufs.AgentToServer
	if err := proto.Unmarshal(none, &noneMsg); err != nil {
		t.Fatalf("unmarshal none: %v", err)
	}
	if noneMsg.GetRemoteConfigStatus() != nil {
		t.Errorf("RemoteConfigStatus should be absent before any config is applied")
	}
}

func TestParseServerToAgent(t *testing.T) {
	cases := []struct {
		name          string
		msg           *protobufs.ServerToAgent
		wantFullState bool
		wantToken     string
		wantCfgBody   string
		wantCfgHash   string
	}{
		{
			name:          "empty ack",
			msg:           &protobufs.ServerToAgent{},
			wantFullState: false,
		},
		{
			name:          "report full state flag",
			msg:           &protobufs.ServerToAgent{Flags: serverToAgentFlagReportFullState},
			wantFullState: true,
		},
		{
			name: "rotation offer carries new token",
			msg: &protobufs.ServerToAgent{
				ConnectionSettings: &protobufs.ConnectionSettingsOffers{
					Opamp: &protobufs.OpAMPConnectionSettings{
						DestinationEndpoint: "https://x",
						Headers: &protobufs.Headers{
							Headers: []*protobufs.Header{
								{Key: "Authorization", Value: "Bearer rotated"},
							},
						},
					},
				},
			},
			wantToken: "Bearer rotated",
		},
		{
			name: "remote config delivery",
			msg: &protobufs.ServerToAgent{
				RemoteConfig: &protobufs.AgentRemoteConfig{
					Config: &protobufs.AgentConfigMap{
						ConfigMap: map[string]*protobufs.AgentConfigObject{
							"otelcol.yml": {Body: []byte("service: {}\n"), ContentType: "text/yaml"},
						},
					},
					ConfigHash: []byte{0x01, 0x02},
				},
			},
			wantCfgBody: "service: {}\n",
			wantCfgHash: "\x01\x02",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, err := proto.Marshal(tc.msg)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got, err := parseServerToAgent(body)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got.reportFullState != tc.wantFullState {
				t.Errorf("reportFullState = %v, want %v", got.reportFullState, tc.wantFullState)
			}
			if got.newToken != tc.wantToken {
				t.Errorf("newToken = %q, want %q", got.newToken, tc.wantToken)
			}
			if string(got.remoteConfigBody) != tc.wantCfgBody {
				t.Errorf("remoteConfigBody = %q, want %q", got.remoteConfigBody, tc.wantCfgBody)
			}
			if string(got.remoteConfigHash) != tc.wantCfgHash {
				t.Errorf("remoteConfigHash = %q, want %q", got.remoteConfigHash, tc.wantCfgHash)
			}
		})
	}
}

// identifyingAttr / nonIdentifyingAttr mirror the server's reads so tests assert
// against the same buckets the server consumes.
func identifyingAttr(msg *protobufs.AgentToServer, key string) string {
	for _, kv := range msg.GetAgentDescription().GetIdentifyingAttributes() {
		if kv.GetKey() == key {
			return kv.GetValue().GetStringValue()
		}
	}
	return ""
}

func nonIdentifyingAttr(msg *protobufs.AgentToServer, key string) string {
	for _, kv := range msg.GetAgentDescription().GetNonIdentifyingAttributes() {
		if kv.GetKey() == key {
			return kv.GetValue().GetStringValue()
		}
	}
	return ""
}

func effectiveConfigBody(msg *protobufs.AgentToServer) []byte {
	m := msg.GetEffectiveConfig().GetConfigMap().GetConfigMap()
	for _, f := range m {
		return f.GetBody()
	}
	return nil
}
