// Package agenttest is what tests use to act as a collector's supervisor
// against an agent endpoint's handler: an OpAMP poll's message, sending it
// in-process, and reading what the answer pushed. It holds no server code.
package agenttest

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"

	"github.com/telflo/opamp-server/middleware"
)

// GroupAttribute is the agent description's attribute naming the
// collector's group, which a collector's enrollment needs.
const GroupAttribute = "telflo.collector_group_id"

// SupervisorCapabilities are what a supervisor reports: it reports its
// status and effective configuration, accepts a remote configuration, and
// accepts OpAMP connection settings (the hand-off).
const SupervisorCapabilities = uint64(protobufs.AgentCapabilities_AgentCapabilities_ReportsStatus |
	protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig |
	protobufs.AgentCapabilities_AgentCapabilities_ReportsEffectiveConfig |
	protobufs.AgentCapabilities_AgentCapabilities_ReportsRemoteConfig |
	protobufs.AgentCapabilities_AgentCapabilities_AcceptsOpAMPConnectionSettings)

// Message is a poll's AgentToServer, encoded: the instance uid's raw bytes,
// the sequence number, SupervisorCapabilities and an agent description
// naming the group.
func Message(t testing.TB, uid uuid.UUID, sequence uint64, group string) []byte {
	t.Helper()
	encoded, err := proto.Marshal(&protobufs.AgentToServer{
		InstanceUid: uid[:], SequenceNum: sequence, Capabilities: SupervisorCapabilities,
		AgentDescription: &protobufs.AgentDescription{NonIdentifyingAttributes: []*protobufs.KeyValue{{
			Key: GroupAttribute, Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: group}}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// Request is a poll of message with bearer ("" for none), as a supervisor
// sends it.
func Request(bearer string, message []byte) *http.Request {
	request := httptest.NewRequest(http.MethodPost, middleware.AgentRoute, bytes.NewReader(message))
	request.Header.Set("Content-Type", "application/x-protobuf")
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	return request
}

// Poll sends one poll through handler and answers the reply and its decoded
// ServerToAgent (nil unless 200).
func Poll(t testing.TB, handler http.Handler, bearer string, message []byte) (*httptest.ResponseRecorder, *protobufs.ServerToAgent) {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, Request(bearer, message))
	if recorder.Code != http.StatusOK {
		return recorder, nil
	}
	answer := &protobufs.ServerToAgent{}
	if err := proto.Unmarshal(recorder.Body.Bytes(), answer); err != nil {
		t.Fatalf("the answer isn't a ServerToAgent: %v", err)
	}
	return recorder, answer
}

// PushedBearer is the bearer an answer's connection settings carry, or "".
func PushedBearer(answer *protobufs.ServerToAgent) string {
	for _, header := range answer.GetConnectionSettings().GetOpamp().GetHeaders().GetHeaders() {
		if header.GetKey() == "Authorization" {
			if bearer, found := bytes.CutPrefix([]byte(header.GetValue()), []byte("Bearer ")); found {
				return string(bearer)
			}
		}
	}
	return ""
}

// PushedYAML is the configuration an answer pushes, or "".
func PushedYAML(answer *protobufs.ServerToAgent) string {
	for _, file := range answer.GetRemoteConfig().GetConfig().GetConfigMap() {
		return string(file.GetBody())
	}
	return ""
}
