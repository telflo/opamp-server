package shutdown

import (
	"context"
	"testing"
)

// A context derived from WithForcedStop's, even one detached from its
// cancellation, finds the base context again, and a cancel of the base is
// seen through it; a context without one finds nothing.
func TestForcedStopFindsTheBaseThroughADetachedContext(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	request, cancelRequest := context.WithCancel(WithForcedStop(base))
	defer cancelRequest()
	detached := context.WithoutCancel(request)

	stop, ok := ForcedStop(detached)
	if !ok {
		t.Fatal("a detached request context lost the forced stop")
	}
	cancel()
	select {
	case <-stop.Done():
	default:
		t.Fatal("cancelling the base isn't seen through the forced stop")
	}
	if _, ok := ForcedStop(context.Background()); ok {
		t.Fatal("a context without a forced stop reported one")
	}
}
