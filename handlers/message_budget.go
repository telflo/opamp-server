package handlers

import (
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// The AgentToServer budget: what a body may hold before proto.Unmarshal
// builds it. The body cap (middleware.MaxBodyBytes) bounds bytes, not
// structure, and decoding multiplies structure: an empty KeyValue is two
// bytes on the wire and about 80 in memory, so a 4 MiB body of them decoded
// to 160 MB. A real supervisor's full state holds a few hundred to about
// 7,000 fields (its available components the most: a contrib collector's
// 300 or so, each with its metadata), so these bounds sit over ten times
// above it, and hold a decoded message to about 8 MB.
const (
	// MaxMessageFields is the most fields, at every depth, a message may
	// hold: each occurrence of a repeated or map field counts once.
	MaxMessageFields = 100_000
	// MaxMessageDepth is the most nested messages, below AgentToServer, a
	// message may hold. It also bounds overBudget's own recursion.
	MaxMessageDepth = 64
)

// withinMessageBudget reports whether body, an encoded message of type md,
// holds at most MaxMessageFields fields nested at most MaxMessageDepth deep.
// It reads the wire format only and allocates nothing. A body it can't
// parse is within the budget as far as it read: proto.Unmarshal refuses it
// next, as an invalid message.
func withinMessageBudget(body []byte, md protoreflect.MessageDescriptor) bool {
	fields := MaxMessageFields
	over, _ := overBudget(body, md, 0, &fields)
	return !over
}

// overBudget walks one message's fields, counting each against fields and
// descending into those that are messages (a map's entries are messages
// too). It answers whether the budget ran out, and whether the walk stopped
// early, at the budget or at bytes it can't parse. A field md doesn't
// describe, or one whose wire type isn't its kind's, is skipped as a value:
// proto.Unmarshal keeps it as unknown bytes, not as structure.
func overBudget(b []byte, md protoreflect.MessageDescriptor, depth int, fields *int) (over, stopped bool) {
	if depth > MaxMessageDepth {
		return true, true
	}
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return false, true
		}
		b = b[n:]
		if *fields--; *fields < 0 {
			return true, true
		}
		if fd := md.Fields().ByNumber(num); fd != nil && fd.Message() != nil && typ == protowire.BytesType {
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return false, true
			}
			if over, stopped := overBudget(v, fd.Message(), depth+1, fields); stopped {
				return over, true
			}
			b = b[n:]
			continue
		}
		n = protowire.ConsumeFieldValue(num, typ, b)
		if n < 0 {
			return false, true
		}
		b = b[n:]
	}
	return false, false
}
