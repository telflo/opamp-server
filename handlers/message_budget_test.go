package handlers

import (
	"bytes"
	"fmt"
	"runtime"
	"testing"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/telflo/telflo-opamp-server/middleware"
)

var agentToServer = (&protobufs.AgentToServer{}).ProtoReflect().Descriptor()

// emptyAttributes is an AgentToServer whose agent description holds n empty
// identifying attributes: two bytes each on the wire, about 80 decoded.
func emptyAttributes(n int) []byte {
	desc := bytes.Repeat([]byte{0x0a, 0x00}, n)
	uid := uuid.New()
	b := protowire.AppendTag(nil, 1, protowire.BytesType)
	b = protowire.AppendBytes(b, uid[:])
	b = protowire.AppendTag(b, 3, protowire.BytesType)
	return protowire.AppendBytes(b, desc)
}

// nested is an AgentToServer whose one attribute's value is an array
// nested depth arrays deep.
func nested(depth int) *protobufs.AgentToServer {
	value := &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: "leaf"}}
	for range depth {
		value = &protobufs.AnyValue{Value: &protobufs.AnyValue_ArrayValue{
			ArrayValue: &protobufs.ArrayValue{Values: []*protobufs.AnyValue{value}}}}
	}
	m := message(uuid.New(), 1)
	m.AgentDescription.NonIdentifyingAttributes = []*protobufs.KeyValue{{Key: "deep", Value: value}}
	return m
}

// supervisorMessage is a full state as large as a real supervisor's gets:
// its attributes, a 1 MiB effective configuration (the most the server
// sends, filled in), health for 50 components, and 300 available
// components, each with its metadata and a sub-component.
func supervisorMessage() *protobufs.AgentToServer {
	m := message(uuid.New(), 1)
	for i := range 20 {
		m.AgentDescription.NonIdentifyingAttributes = append(m.AgentDescription.NonIdentifyingAttributes,
			&protobufs.KeyValue{Key: fmt.Sprintf("attribute.%d", i),
				Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: "value"}}})
	}
	m.EffectiveConfig = &protobufs.EffectiveConfig{ConfigMap: &protobufs.AgentConfigMap{
		ConfigMap: map[string]*protobufs.AgentConfigObject{"": {Body: bytes.Repeat([]byte("a"), 1<<20), ContentType: "text/yaml"}}}}
	m.Health = &protobufs.ComponentHealth{Healthy: true, Status: "StatusOK", ComponentHealthMap: map[string]*protobufs.ComponentHealth{}}
	for i := range 50 {
		m.Health.ComponentHealthMap[fmt.Sprintf("pipeline:traces/%d", i)] = &protobufs.ComponentHealth{Healthy: true, Status: "StatusOK"}
	}
	m.AvailableComponents = &protobufs.AvailableComponents{Components: map[string]*protobufs.ComponentDetails{}, Hash: []byte("hash")}
	for i := range 300 {
		kv := func(k string) *protobufs.KeyValue {
			return &protobufs.KeyValue{Key: k, Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: "v"}}}
		}
		m.AvailableComponents.Components[fmt.Sprintf("receiver/%d", i)] = &protobufs.ComponentDetails{
			Metadata:        []*protobufs.KeyValue{kv("code.namespace"), kv("version"), kv("stability")},
			SubComponentMap: map[string]*protobufs.ComponentDetails{"sub": {Metadata: []*protobufs.KeyValue{kv("version")}}},
		}
	}
	return m
}

// A body of millions of empty fields, under the cap, is over the budget,
// and checking it allocates nothing: the memory it would have taken to
// decode is never spent.
func TestTheBudgetRefusesABodyThatDecodingWouldMultiply(t *testing.T) {
	body := emptyAttributes((middleware.MaxBodyBytes - 64) / 2)
	if len(body) > middleware.MaxBodyBytes {
		t.Fatalf("the body is %d bytes, over the cap", len(body))
	}
	if withinMessageBudget(body, agentToServer) {
		t.Fatal("a body of 2 million empty attributes is within the budget")
	}
	if allocs := testing.AllocsPerRun(5, func() { withinMessageBudget(body, agentToServer) }); allocs != 0 {
		t.Fatalf("the check allocated %v times; want 0", allocs)
	}
}

// Every field counts, at every depth: MaxMessageFields of them fit, one
// more doesn't. The body's two own fields (the uid and the description)
// count too.
func TestTheBudgetCountsEveryField(t *testing.T) {
	if !withinMessageBudget(emptyAttributes(MaxMessageFields-2), agentToServer) {
		t.Fatal("a message of MaxMessageFields fields is over the budget")
	}
	if withinMessageBudget(emptyAttributes(MaxMessageFields-1), agentToServer) {
		t.Fatal("a message of MaxMessageFields+1 fields is within the budget")
	}
}

// Nesting up to MaxMessageDepth fits, and past it doesn't. The attribute's
// value is three levels down (the description, the attribute, its value),
// and each array adds two (the array and its value): 30 arrays put the
// innermost value at 63, and 31 at 65.
func TestTheBudgetBoundsTheNesting(t *testing.T) {
	for _, tc := range []struct {
		arrays int
		within bool
	}{{(MaxMessageDepth - 4) / 2, true}, {(MaxMessageDepth - 2) / 2, false}} {
		body, err := proto.Marshal(nested(tc.arrays))
		if err != nil {
			t.Fatal(err)
		}
		if got := withinMessageBudget(body, agentToServer); got != tc.within {
			t.Errorf("%d nested arrays: within the budget %v; want %v", tc.arrays, got, tc.within)
		}
	}
}

// A full state as large as a real supervisor's is well within the budget,
// at a tenth of it or less, and under the body cap.
func TestTheBudgetFitsARealSupervisorsFullState(t *testing.T) {
	body, err := proto.Marshal(supervisorMessage())
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > middleware.MaxBodyBytes {
		t.Fatalf("the message is %d bytes, over the cap", len(body))
	}
	fields := MaxMessageFields
	if over, _ := overBudget(body, agentToServer, 0, &fields); over || MaxMessageFields-fields > MaxMessageFields/10 {
		t.Fatalf("a real full state used %d of %d fields; want a tenth or less", MaxMessageFields-fields, MaxMessageFields)
	}
}

// A body the walk can't parse is left to proto.Unmarshal, which refuses it
// as an invalid message, not as over the budget.
func TestTheBudgetLeavesAnUnparseableBodyToTheDecoder(t *testing.T) {
	body := []byte("\xff\xff\xff not an AgentToServer")
	if !withinMessageBudget(body, agentToServer) {
		t.Fatal("an unparseable body is over the budget")
	}
	if err := proto.Unmarshal(body, &protobufs.AgentToServer{}); err == nil {
		t.Fatal("proto.Unmarshal accepted the unparseable body")
	}
}

// The decoded size the budget bounds: at MaxMessageFields empty
// attributes, a decoded message takes about 8 MB, where the cap's worth of
// them took over 160.
func TestTheBudgetHoldsADecodedMessageToMegabytesNotHundreds(t *testing.T) {
	body := emptyAttributes(MaxMessageFields - 2)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	m := &protobufs.AgentToServer{}
	if err := proto.Unmarshal(body, m); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if grown := after.TotalAlloc - before.TotalAlloc; grown > 16<<20 {
		t.Fatalf("decoding a message at the budget allocated %d MB; want under 16", grown>>20)
	}
	runtime.KeepAlive(m)
}
