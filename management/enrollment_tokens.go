package management

import (
	"errors"
	"net/http"

	"github.com/telflo/opamp-server/store/enrollmenttokens"
	"github.com/telflo/opamp-server/tokens"
)

// The enrollment token routes. A bearer is never stored: rotate and reveal
// mint a fresh one under the current key, and each is audited (`rotated`
// and `issued`, or `viewed`); a revoke is audited (`revoked`). The audit
// rows hold ids and the actor only. The literal rotate and default paths
// are registered before the token id's paths.
const (
	enrollmentTokensPath = fleetPath + "/tokens"
	rotatePath           = enrollmentTokensPath + "/rotate"
	issueDefaultPath     = enrollmentTokensPath + "/default"
	tokenEventsPath      = enrollmentTokensPath + "/events"
	revealPath           = enrollmentTokensPath + "/{tokenId}/reveal"
	revokePath           = enrollmentTokensPath + "/{tokenId}/revoke"
)

// The token routes' fixed messages.
const (
	messageEnrollmentTokenNotFound = "The fleet has no enrollment token with this id."
	messageEnrollmentTokenRevoked  = "The enrollment token is revoked."
	messageEnrollmentTokenExpired  = "The enrollment token has expired."
	messageFleetArchived           = "The fleet is archived."
	messageDefaultTokenExists      = "The fleet already has a valid default token."
)

func (s *Server) registerEnrollmentTokens() {
	s.api.handle("GET "+enrollmentTokensPath, getOnly(s.listEnrollmentTokens))
	s.api.handle("GET "+tokenEventsPath, getOnly(s.listEnrollmentTokenEvents))
	s.api.handle("POST "+rotatePath, http.HandlerFunc(s.rotateEnrollmentToken))
	s.api.handle("POST "+issueDefaultPath, http.HandlerFunc(s.issueDefaultEnrollmentToken))
	s.api.handle("POST "+revealPath, http.HandlerFunc(s.revealEnrollmentToken))
	s.api.handle("POST "+revokePath, http.HandlerFunc(s.revokeEnrollmentToken))
}

// listEnrollmentTokens is GET …/tokens: 200 with the fleet's tokens, newest
// first, each with how many of the fleet's collectors were last seen on it
// in the last 5 minutes (no bearer); or 404 fleet_not_found.
func (s *Server) listEnrollmentTokens(w http.ResponseWriter, r *http.Request) {
	organizationID, fleetID, ok := s.organizationAndFleet(w, r)
	if !ok {
		return
	}
	listed, err := s.enrollmentTokens.List(r.Context(), organizationID, fleetID, s.clock())
	switch {
	case errors.Is(err, enrollmenttokens.ErrFleetNotFound):
		writeFleetNotFound(w)
		return
	case err != nil:
		s.storeFailed(w, r, storeEnrollmentTokens, err)
		return
	}
	reply := enrollmentTokenListReply{Tokens: make([]listedEnrollmentTokenReply, 0, len(listed))}
	for _, l := range listed {
		reply.Tokens = append(reply.Tokens, listedEnrollmentTokenReply{
			enrollmentTokenReply: enrollmentTokenJSON(l.Token), ActiveCollectors: l.ActiveCollectors,
		})
	}
	writeJSON(w, http.StatusOK, reply)
}

// listEnrollmentTokenEvents is GET …/tokens/events?limit=…&cursor=…: 200
// with a page of the fleet's token events (issued, viewed, rotated,
// revoked; ids, the actor and the time, never a bearer or a kind), newest
// first; 400 for a limit or a cursor; 404 fleet_not_found.
func (s *Server) listEnrollmentTokenEvents(w http.ResponseWriter, r *http.Request) {
	organizationID, fleetID, ok := s.organizationAndFleet(w, r)
	if !ok {
		return
	}
	query, rerr := parseQuery(r.URL.RawQuery, "limit", "cursor")
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	limit, rerr := listLimit(query, enrollmenttokens.MaxListLimit)
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	var after *enrollmenttokens.EventCursor
	if raw, present := query["cursor"]; present {
		cursor, err := enrollmenttokens.ParseEventCursor(raw)
		if err != nil {
			writeRequestError(w, invalid(messageCursor))
			return
		}
		after = &cursor
	}
	page, err := s.enrollmentTokens.ListEvents(r.Context(), organizationID, fleetID, limit, after)
	switch {
	case errors.Is(err, enrollmenttokens.ErrFleetNotFound):
		writeFleetNotFound(w)
		return
	case err != nil:
		s.storeFailed(w, r, storeEnrollmentTokens, err)
		return
	}
	reply := tokenEventListReply{Events: make([]tokenEventReply, 0, len(page.Events))}
	for _, e := range page.Events {
		reply.Events = append(reply.Events, tokenEventReply{
			ID: e.ID.String(), TokenID: e.TokenID.String(), Event: string(e.Kind),
			Actor: actorJSON(e.Actor), OccurredAt: formatTime(e.OccurredAt),
		})
	}
	if page.Next != nil {
		next := page.Next.String()
		reply.NextCursor = &next
	}
	writeJSON(w, http.StatusOK, reply)
}

// rotateEnrollmentToken is POST …/tokens/rotate with {"expiresAt",
// "graceEndsAt", "actor"}: under the fleet's lock, the fleet's current
// rotate token's life ends at the grace end (or its own expiry, if sooner)
// and it is marked to push its successor; a new rotate token is made. 201
// with the new token's bearer; 404 fleet_not_found; 409 fleet_archived.
func (s *Server) rotateEnrollmentToken(w http.ResponseWriter, r *http.Request) {
	organizationID, fleetID, ok := s.organizationAndFleet(w, r)
	if !ok {
		return
	}
	body, ok := s.readJSON(w, r)
	if !ok {
		return
	}
	now := s.clock()
	rotation, rerr := decodeRotation(body, now)
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	rotation.OrganizationID, rotation.FleetID, rotation.At = organizationID, fleetID, now
	result, err := s.enrollmentTokens.Rotate(r.Context(), rotation)
	switch {
	case errors.Is(err, enrollmenttokens.ErrFleetNotFound):
		writeFleetNotFound(w)
		return
	case errors.Is(err, enrollmenttokens.ErrFleetArchived):
		writeError(w, http.StatusConflict, codeFleetArchived, messageFleetArchived)
		return
	case errors.Is(err, enrollmenttokens.ErrInvalid):
		writeRequestError(w, invalid(messageUnstorable))
		return
	case err != nil:
		s.storeFailed(w, r, storeEnrollmentTokens, err)
		return
	}
	bearer, err := s.keys.Mint(tokens.Claims{TokenID: result.Token.ID, FleetID: fleetID})
	if err != nil {
		s.mintFailed(w, r)
		return
	}
	reply := rotatedReply{
		ID: result.Token.ID.String(), Bearer: bearer, ExpiresAt: formatOptionalTime(result.Token.ExpiresAt),
	}
	if result.PredecessorID != nil {
		predecessor := result.PredecessorID.String()
		reply.PredecessorID = &predecessor
	}
	writeBearerJSON(w, http.StatusCreated, reply)
}

// issueDefaultEnrollmentToken is POST …/tokens/default with {"actor"}:
// under the fleet's lock, a new default token when every one the fleet had
// is revoked, and an `issued` event. 201 with its bearer (a reveal's
// shape); 404 fleet_not_found; 409 fleet_archived, then 409
// enrollment_token_default_exists while a default token is valid (a second
// issue racing the first answers so too).
func (s *Server) issueDefaultEnrollmentToken(w http.ResponseWriter, r *http.Request) {
	organizationID, fleetID, ok := s.organizationAndFleet(w, r)
	if !ok {
		return
	}
	body, ok := s.readJSON(w, r)
	if !ok {
		return
	}
	actor, rerr := decodeActorOnly(body)
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	issued, err := s.enrollmentTokens.IssueDefault(r.Context(), organizationID, fleetID, actor, s.clock())
	switch {
	case errors.Is(err, enrollmenttokens.ErrFleetNotFound):
		writeFleetNotFound(w)
		return
	case errors.Is(err, enrollmenttokens.ErrFleetArchived):
		writeError(w, http.StatusConflict, codeFleetArchived, messageFleetArchived)
		return
	case errors.Is(err, enrollmenttokens.ErrDefaultExists):
		writeError(w, http.StatusConflict, codeEnrollmentTokenDefaultExists, messageDefaultTokenExists)
		return
	case errors.Is(err, enrollmenttokens.ErrInvalid):
		writeRequestError(w, invalid(messageUnstorable))
		return
	case err != nil:
		s.storeFailed(w, r, storeEnrollmentTokens, err)
		return
	}
	bearer, err := s.keys.Mint(tokens.Claims{TokenID: issued.ID, FleetID: fleetID})
	if err != nil {
		s.mintFailed(w, r)
		return
	}
	writeBearerJSON(w, http.StatusCreated, revealedReply{
		ID: issued.ID.String(), Kind: string(issued.Kind), Bearer: bearer, ExpiresAt: formatOptionalTime(issued.ExpiresAt),
	})
}

// revealEnrollmentToken is POST …/tokens/{tokenId}/reveal with {"actor"}:
// 200 with a fresh bearer for a valid token, and a `viewed` event; 404; 409
// enrollment_token_revoked or enrollment_token_expired, writing nothing. A
// token of an archived fleet is revealed too: a reveal is a view, and the
// fleet may be unarchived.
// The bearer is minted before the event is written, so no `viewed` is
// recorded for a bearer nobody got.
func (s *Server) revealEnrollmentToken(w http.ResponseWriter, r *http.Request) {
	ids, ok := s.underOrganization(w, r, "fleetId", "tokenId")
	if !ok {
		return
	}
	organizationID, fleetID, credentialID := ids[0], ids[1], ids[2]
	body, ok := s.readJSON(w, r)
	if !ok {
		return
	}
	actor, rerr := decodeActorOnly(body)
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	bearer, err := s.keys.Mint(tokens.Claims{TokenID: credentialID, FleetID: fleetID})
	if err != nil {
		s.mintFailed(w, r)
		return
	}
	viewed, err := s.enrollmentTokens.RecordViewed(r.Context(), organizationID, fleetID, credentialID, actor, s.clock())
	switch {
	case errors.Is(err, enrollmenttokens.ErrNotFound):
		s.notFoundOrFleetNotFound(w, r, organizationID, fleetID, codeEnrollmentTokenNotFound, messageEnrollmentTokenNotFound)
	case errors.Is(err, enrollmenttokens.ErrRevoked):
		writeError(w, http.StatusConflict, codeEnrollmentTokenRevoked, messageEnrollmentTokenRevoked)
	case errors.Is(err, enrollmenttokens.ErrExpired):
		writeError(w, http.StatusConflict, codeEnrollmentTokenExpired, messageEnrollmentTokenExpired)
	case errors.Is(err, enrollmenttokens.ErrInvalid):
		writeRequestError(w, invalid(messageUnstorable))
	case err != nil:
		s.storeFailed(w, r, storeEnrollmentTokens, err)
	default:
		writeBearerJSON(w, http.StatusOK, revealedReply{
			ID: viewed.ID.String(), Kind: string(viewed.Kind), Bearer: bearer, ExpiresAt: formatOptionalTime(viewed.ExpiresAt),
		})
	}
}

// revokeEnrollmentToken is POST …/tokens/{tokenId}/revoke with {"actor"}:
// 200 with the token, revoked now with a `revoked` event; a token already
// revoked answers 200 with it as it is, changing nothing and writing no
// event (a retry is safe); 404. The default token may be revoked too.
func (s *Server) revokeEnrollmentToken(w http.ResponseWriter, r *http.Request) {
	ids, ok := s.underOrganization(w, r, "fleetId", "tokenId")
	if !ok {
		return
	}
	organizationID, fleetID, credentialID := ids[0], ids[1], ids[2]
	body, ok := s.readJSON(w, r)
	if !ok {
		return
	}
	actor, rerr := decodeActorOnly(body)
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	_, revoked, err := s.enrollmentTokens.Revoke(r.Context(), organizationID, fleetID, credentialID, actor, s.clock())
	switch {
	case errors.Is(err, enrollmenttokens.ErrNotFound):
		s.notFoundOrFleetNotFound(w, r, organizationID, fleetID, codeEnrollmentTokenNotFound, messageEnrollmentTokenNotFound)
	case errors.Is(err, enrollmenttokens.ErrInvalid):
		writeRequestError(w, invalid(messageUnstorable))
	case err != nil:
		s.storeFailed(w, r, storeEnrollmentTokens, err)
	default:
		writeJSON(w, http.StatusOK, enrollmentTokenJSON(revoked))
	}
}
