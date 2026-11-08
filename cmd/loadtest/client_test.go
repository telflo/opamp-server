package main

import (
	"bytes"
	"context"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"
)

// capturedRequest is what the scripted server records off each poll so the test
// can assert the client's lifecycle progression.
type capturedRequest struct {
	auth      string
	rcsHash   []byte
	rcsStatus protobufs.RemoteConfigStatuses
	effective []byte
	fullState bool // AgentDescription present
}

// TestVirtualSupervisor_Lifecycle drives the real poll loop against a scripted
// server that reproduces the server-supervisor handshake: the first (default
// token) poll is answered with ReportFullState + a rotation offer; the next poll
// (now on the rotated token) is answered with a remote config; a later poll is
// answered with ReportFullState again to force a full-state re-report. It asserts
// the client switches tokens, stores the delivered config, and thereafter
// reports RemoteConfigStatus{APPLIED, hash} and the delivered config as its
// effective state.
func TestVirtualSupervisor_Lifecycle(t *testing.T) {
	const (
		defaultToken = "Bearer default"
		rotatedToken = "Bearer rotated"
	)
	deliveredBody := []byte("service:\n  pipelines: {logs: {}}\n")
	deliveredHash := []byte{0xab, 0xcd, 0xef}

	rotationResp := mustMarshal(t, &protobufs.ServerToAgent{
		Flags: serverToAgentFlagReportFullState,
		ConnectionSettings: &protobufs.ConnectionSettingsOffers{
			Opamp: &protobufs.OpAMPConnectionSettings{
				DestinationEndpoint: "https://rotate",
				Headers: &protobufs.Headers{
					Headers: []*protobufs.Header{{Key: "Authorization", Value: rotatedToken}},
				},
			},
		},
	})
	configResp := mustMarshal(t, &protobufs.ServerToAgent{
		RemoteConfig: &protobufs.AgentRemoteConfig{
			Config: &protobufs.AgentConfigMap{
				ConfigMap: map[string]*protobufs.AgentConfigObject{
					"otelcol.yml": {Body: deliveredBody, ContentType: "text/yaml"},
				},
			},
			ConfigHash: deliveredHash,
		},
	})
	fullStateResp := mustMarshal(t, &protobufs.ServerToAgent{Flags: serverToAgentFlagReportFullState})
	emptyResp := mustMarshal(t, &protobufs.ServerToAgent{})

	var (
		mu   sync.Mutex
		reqs []capturedRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var msg protobufs.AgentToServer
		_ = proto.Unmarshal(body, &msg)

		mu.Lock()
		n := len(reqs)
		reqs = append(reqs, capturedRequest{
			auth:      r.Header.Get("Authorization"),
			rcsHash:   msg.GetRemoteConfigStatus().GetLastRemoteConfigHash(),
			rcsStatus: msg.GetRemoteConfigStatus().GetStatus(),
			effective: effectiveConfigBody(&msg),
			fullState: msg.GetAgentDescription() != nil,
		})
		mu.Unlock()

		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
		switch n {
		case 0:
			w.Write(rotationResp)
		case 1:
			w.Write(configResp)
		case 2:
			w.Write(fullStateResp)
		default:
			w.Write(emptyResp)
		}
	}))
	defer srv.Close()

	metrics := &Collector{}
	vs := &VirtualSupervisor{
		name:           "col-0001",
		token:          defaultToken,
		instanceUID:    newInstanceUID(),
		hostName:       "col-0001",
		hostIP:         "10.42.0.1",
		groupID:        "grp-1",
		endpoint:       srv.URL,
		deployInterval: 10 * time.Millisecond,
		pollInterval:   10 * time.Millisecond,
		jitter:         0.1,
		client:         srv.Client(),
		metrics:        metrics,
		rng:            rand.New(rand.NewSource(1)),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	vs.Run(ctx)

	mu.Lock()
	defer mu.Unlock()
	if len(reqs) < 4 {
		t.Fatalf("only %d polls reached the server, need at least 4 to observe the lifecycle", len(reqs))
	}

	if reqs[0].auth != defaultToken {
		t.Errorf("first poll auth = %q, want %q", reqs[0].auth, defaultToken)
	}
	if !anyReq(reqs, func(c capturedRequest) bool { return c.auth == rotatedToken }) {
		t.Errorf("no poll used the rotated token %q — token was not adopted", rotatedToken)
	}
	if anyReq(reqs[1:], func(c capturedRequest) bool { return c.auth == defaultToken }) {
		t.Errorf("a poll after the handoff still used the default token — client did not switch")
	}
	if !anyReq(reqs, func(c capturedRequest) bool {
		return bytes.Equal(c.rcsHash, deliveredHash) &&
			c.rcsStatus == protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED
	}) {
		t.Errorf("no poll reported RemoteConfigStatus{APPLIED, %x} after config delivery", deliveredHash)
	}
	if !anyReq(reqs, func(c capturedRequest) bool {
		return c.fullState && bytes.Equal(c.effective, deliveredBody)
	}) {
		t.Errorf("no full-state poll reported the delivered config as its effective state")
	}

	s := metrics.Snapshot()
	if s.Rotations != 1 {
		t.Errorf("Rotations = %d, want 1", s.Rotations)
	}
	if s.Configs != 1 {
		t.Errorf("Configs = %d, want 1", s.Configs)
	}
	if s.Status2xx == 0 {
		t.Errorf("expected 2xx responses, got %d", s.Status2xx)
	}
}

func anyReq(reqs []capturedRequest, pred func(capturedRequest) bool) bool {
	for _, c := range reqs {
		if pred(c) {
			return true
		}
	}
	return false
}

func mustMarshal(t *testing.T, msg proto.Message) []byte {
	t.Helper()
	b, err := proto.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
