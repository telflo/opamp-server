// Package shutdown carries a server's forced stop in its requests'
// contexts. The listeners (package serve) give every request a context that
// only a forced stop cancels; work that must outlive its caller, such as the
// management API's long deletes, detaches from the request's cancellation
// but still asks ForcedStop for that context, so a forced stop cuts it off
// with every other request.
package shutdown

import "context"

// forcedStopKey is the context value under which WithForcedStop puts the
// context a forced stop cancels.
type forcedStopKey struct{}

// WithForcedStop is base, carrying itself as the context a forced stop
// cancels: every request context derived from it can find base again with
// ForcedStop, even once detached from its own cancellation.
func WithForcedStop(base context.Context) context.Context {
	return context.WithValue(base, forcedStopKey{}, base)
}

// ForcedStop is the context a forced stop cancels, as WithForcedStop put it
// in ctx (or any context derived from it, its values kept), and whether ctx
// carries one.
func ForcedStop(ctx context.Context) (context.Context, bool) {
	stop, ok := ctx.Value(forcedStopKey{}).(context.Context)
	return stop, ok
}
