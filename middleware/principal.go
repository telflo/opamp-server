package middleware

import (
	"context"

	"github.com/google/uuid"

	"github.com/telflo/opamp-server/store/enrollmenttokens"
	"github.com/telflo/opamp-server/store/fleets"
)

// Principal is who an agent request is, as the authentication middleware
// established it: one value in the request's context. Every field but
// ClientIP comes from the token's row (and its fleet's, joined), never from
// the bearer: the bearer's fleet is only compared with the row's.
type Principal struct {
	// OrganizationID is the token row's organization.
	OrganizationID uuid.UUID
	// FleetID is the token row's fleet (equal to the bearer's, or the
	// request was refused).
	FleetID uuid.UUID
	// FleetType is the fleet's type: server-class, or the request was
	// refused. Read by nothing.
	FleetType fleets.Type
	// TokenID is the token's id, the bearer's token_id and the row's key.
	TokenID uuid.UUID
	// TokenKind is the token's kind: a default token is served no
	// configuration and is pushed the fleet's rotate token.
	TokenKind enrollmenttokens.Kind
	// DoRefresh is the row's do_refresh: a rotated token in its grace, pushed
	// the fleet's newest rotate token.
	DoRefresh bool
	// ClientIP is the collector's address as the request shows it (the
	// ClientIP function): for display only, never a trust or an
	// authentication input.
	ClientIP string
}

// principalKey is the context key of the request's Principal.
type principalKey struct{}

// WithPrincipal returns ctx carrying p.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the request's Principal, and whether there is one
// (none when the request didn't pass the authentication middleware).
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}
