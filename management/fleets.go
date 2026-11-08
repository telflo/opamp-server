package management

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/google/uuid"

	"github.com/telflo/opamp-server/store/collectorgroups"
	"github.com/telflo/opamp-server/store/fleets"
	"github.com/telflo/opamp-server/tokens"
)

// The fleet routes. api/openapi.yaml documents each, and the conformance
// test fails on a route registered here that it doesn't. Every route under
// a fleet reads its path ids first (400), then answers 404
// organization_not_found for an organization that isn't registered, before
// anything else; another organization's fleet, group or token is the same
// 404 as one that doesn't exist.
//
// A fleet's reads (the list and one fleet) carry its summary: its
// collectors counted by liveness and by sync at the server's clock, with
// their latest last seen, one grouped query per page; and its rotate
// tokens' state at the same clock, a second grouped query.
const (
	fleetsPath = apiPrefix + "v1/organizations/{organizationId}/fleets"
	fleetPath  = fleetsPath + "/{fleetId}"
)

// The fleet routes' fixed messages. None quotes a value: no label, no id.
const (
	messageFleetNotFound   = "The organization has no fleet with this id."
	messageFleetConflict   = "A different fleet already has this id."
	messageFleetLabelTaken = "Another of the organization's fleets has this label."
	messageMintFailed      = "couldn't mint a bearer"
)

// registerFleets registers the fleet, group and token routes on the API's
// router, behind authentication and the body cap.
func (s *Server) registerFleets() {
	s.api.handle("POST "+fleetsPath, http.HandlerFunc(s.createFleet))
	s.api.handle("GET "+fleetsPath, getOnly(s.listFleets))
	s.api.handle("GET "+fleetPath, getOnly(s.getFleet))
	s.api.handle("PATCH "+fleetPath, http.HandlerFunc(s.updateFleet))
	s.api.handle("DELETE "+fleetPath, http.HandlerFunc(s.deleteFleet))
	s.registerCollectorGroups()
	s.registerEnrollmentTokens()
	s.registerCollectors()
}

// createFleet is POST …/fleets with {"id"?, "label", "type",
// "rotateExpiresAt", "actor"}: 201 with the fleet, its Default group and
// both tokens' bearers, made in one transaction; 200 with the fleet alone
// when the organization already holds this id with this label and type (a
// safe retry: reveal the bearers); 409 fleet_conflict for an id taken
// otherwise, fleet_label_taken for a label taken.
func (s *Server) createFleet(w http.ResponseWriter, r *http.Request) {
	organizationID, rerr := pathUUID(r, "organizationId")
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	if !s.requireOrganization(w, r, organizationID) {
		return
	}
	body, ok := s.readJSON(w, r)
	if !ok {
		return
	}
	now := s.clock()
	request, hasID, rerr := decodeFleetCreate(body, now)
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	if !hasID {
		// Without an id the server makes one, so such a POST can't be
		// safely retried (as an organization's).
		generated, err := uuid.NewRandom()
		if err != nil {
			s.logger.Error("couldn't generate a fleet id", "route", s.routePattern(r))
			writeError(w, http.StatusInternalServerError, codeInternalError, messageInternalError)
			return
		}
		request.ID = generated
	}
	request.OrganizationID = organizationID
	request.At = now

	result, creation, err := s.fleets.Create(r.Context(), request)
	switch {
	case errors.Is(err, fleets.ErrOrganizationNotFound):
		writeOrganizationNotFound(w)
		return
	case errors.Is(err, fleets.ErrInvalid):
		writeRequestError(w, invalid(messageUnstorable))
		return
	case err != nil:
		s.storeFailed(w, r, storeFleets, err)
		return
	}
	switch result {
	case fleets.Created:
	case fleets.Unchanged:
		writeJSON(w, http.StatusOK, fleetUnchangedReply{Fleet: fleetJSON(creation.Fleet)})
		return
	case fleets.LabelTaken:
		writeError(w, http.StatusConflict, codeFleetLabelTaken, messageFleetLabelTaken)
		return
	default:
		writeError(w, http.StatusConflict, codeFleetConflict, messageFleetConflict)
		return
	}

	fleetID := creation.Fleet.ID
	defaultBearer, err := s.keys.Mint(tokens.Claims{TokenID: creation.DefaultTokenID, FleetID: fleetID})
	if err != nil {
		s.mintFailed(w, r)
		return
	}
	rotateBearer, err := s.keys.Mint(tokens.Claims{TokenID: creation.RotateTokenID, FleetID: fleetID})
	if err != nil {
		s.mintFailed(w, r)
		return
	}
	reply := fleetCreatedReply{
		Fleet: fleetJSON(creation.Fleet),
		// The Default group as the store made it, with the fleet: named
		// "Default", nothing assigned, no annotations.
		DefaultGroup: groupJSON(collectorgroups.Group{
			ID: creation.DefaultGroupID, OrganizationID: organizationID, FleetID: fleetID,
			Name: fleets.DefaultGroupName, IsDefault: true,
			CreatedAt: creation.Fleet.CreatedAt, UpdatedAt: creation.Fleet.CreatedAt,
		}),
	}
	reply.Tokens.Default = bearerReply{ID: creation.DefaultTokenID.String(), Bearer: defaultBearer}
	reply.Tokens.Rotate = rotateBearerReply{
		ID: creation.RotateTokenID.String(), Bearer: rotateBearer, ExpiresAt: formatOptionalTime(creation.RotateExpiresAt),
	}
	writeBearerJSON(w, http.StatusCreated, reply)
}

// listFleets is GET …/fleets?status=…&limit=…&cursor=…: 200 with a page of
// the organization's fleets, newest first.
func (s *Server) listFleets(w http.ResponseWriter, r *http.Request) {
	organizationID, rerr := pathUUID(r, "organizationId")
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	if !s.requireOrganization(w, r, organizationID) {
		return
	}
	options, rerr := decodeFleetListQuery(r.URL.RawQuery)
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	page, err := s.fleets.List(r.Context(), organizationID, options)
	if err != nil {
		s.storeFailed(w, r, storeFleets, err)
		return
	}
	ids := make([]uuid.UUID, 0, len(page.Fleets))
	for _, f := range page.Fleets {
		ids = append(ids, f.ID)
	}
	now := s.clock()
	summaries, err := s.collectors.SummariesForFleets(r.Context(), organizationID, ids, now)
	if err != nil {
		s.storeFailed(w, r, storeCollectors, err)
		return
	}
	states, err := s.enrollmentTokens.RotateStates(r.Context(), organizationID, ids, now)
	if err != nil {
		s.storeFailed(w, r, storeEnrollmentTokens, err)
		return
	}
	reply := fleetListReply{Fleets: make([]fleetDetailReply, 0, len(page.Fleets))}
	for _, f := range page.Fleets {
		// A fleet deleted since the page was read has no summary: zero,
		// and no rotate token.
		reply.Fleets = append(reply.Fleets, fleetDetailJSON(f, summaries[f.ID], states))
	}
	if page.Next != nil {
		next := page.Next.String()
		reply.NextCursor = &next
	}
	writeJSON(w, http.StatusOK, reply)
}

// getFleet is GET …/fleets/{fleetId}: 200 with the fleet and its summary,
// or 404.
func (s *Server) getFleet(w http.ResponseWriter, r *http.Request) {
	organizationID, fleetID, ok := s.organizationAndFleet(w, r)
	if !ok {
		return
	}
	stored, err := s.fleets.Get(r.Context(), organizationID, fleetID)
	switch {
	case errors.Is(err, fleets.ErrNotFound):
		writeFleetNotFound(w)
		return
	case err != nil:
		s.storeFailed(w, r, storeFleets, err)
		return
	}
	now := s.clock()
	summaries, err := s.collectors.SummariesForFleets(r.Context(), organizationID, []uuid.UUID{fleetID}, now)
	if err != nil {
		s.storeFailed(w, r, storeCollectors, err)
		return
	}
	states, err := s.enrollmentTokens.RotateStates(r.Context(), organizationID, []uuid.UUID{fleetID}, now)
	if err != nil {
		s.storeFailed(w, r, storeEnrollmentTokens, err)
		return
	}
	writeJSON(w, http.StatusOK, fleetDetailJSON(stored, summaries[fleetID], states))
}

// updateFleet is PATCH …/fleets/{fleetId} with {"label"?, "status"?}, at
// least one: 200 with the fleet (updatedAt moves only when something
// changed), 404, or 409 fleet_label_taken.
func (s *Server) updateFleet(w http.ResponseWriter, r *http.Request) {
	organizationID, fleetID, ok := s.organizationAndFleet(w, r)
	if !ok {
		return
	}
	body, ok := s.readJSON(w, r)
	if !ok {
		return
	}
	changes, rerr := decodeFleetUpdate(body)
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	stored, err := s.fleets.Update(r.Context(), organizationID, fleetID, changes, s.clock())
	switch {
	case errors.Is(err, fleets.ErrNotFound):
		writeFleetNotFound(w)
	case errors.Is(err, fleets.ErrLabelTaken):
		writeError(w, http.StatusConflict, codeFleetLabelTaken, messageFleetLabelTaken)
	case errors.Is(err, fleets.ErrInvalid):
		writeRequestError(w, invalid(messageUnstorable))
	case err != nil:
		s.storeFailed(w, r, storeFleets, err)
	default:
		writeJSON(w, http.StatusOK, fleetJSON(stored))
	}
}

// deleteFleet is DELETE …/fleets/{fleetId}: 204, the fleet and everything
// it holds gone (its groups, tokens and their events, and collectors,
// which frees their slots under the cap), or 404. Once started
// (after the organization's check) it finishes even if the caller hangs
// up, within DeleteTimeout (deletes.go): a retry then answers 404. While
// this replica is already deleting the fleet, or its organization, a
// second delete answers 503 delete_in_progress at once, once the path ids
// are read and before any query (deletes.go).
func (s *Server) deleteFleet(w http.ResponseWriter, r *http.Request) {
	organizationID, rerr := pathUUID(r, "organizationId")
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	fleetID, rerr := pathUUID(r, "fleetId")
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	done, started := s.deleting.startFleet(organizationID, fleetID)
	if !started {
		s.writeDeleteInProgress(w, r)
		return
	}
	defer done()
	if !s.requireOrganization(w, r, organizationID) {
		return
	}
	ctx, cancel := s.deleteContext(r)
	defer cancel()
	deleted, err := s.fleets.Delete(ctx, organizationID, fleetID)
	switch {
	case err != nil:
		s.deleteFailed(w, r, storeFleets, ctx, err)
	case !deleted:
		writeFleetNotFound(w)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// organizationAndFleet reads the organization's and the fleet's path ids
// (400), then requires the organization (404 organization_not_found). On
// failure it has answered.
func (s *Server) organizationAndFleet(w http.ResponseWriter, r *http.Request) (organizationID, fleetID uuid.UUID, ok bool) {
	ids, ok := s.underOrganization(w, r, "fleetId")
	if !ok {
		return uuid.Nil, uuid.Nil, false
	}
	return ids[0], ids[1], true
}

// underOrganization reads the organization's path id and then each named
// one, in order, answering 400 for the first that isn't a UUID; then it
// requires the organization (404 organization_not_found). It returns the
// organization's id followed by the others; on failure it has answered.
func (s *Server) underOrganization(w http.ResponseWriter, r *http.Request, names ...string) ([]uuid.UUID, bool) {
	ids := make([]uuid.UUID, 0, 1+len(names))
	for _, name := range append([]string{"organizationId"}, names...) {
		id, rerr := pathUUID(r, name)
		if rerr != nil {
			writeRequestError(w, rerr)
			return nil, false
		}
		ids = append(ids, id)
	}
	if !s.requireOrganization(w, r, ids[0]) {
		return nil, false
	}
	return ids, true
}

// readJSON checks the Content-Type and reads the capped body; on failure it
// has answered.
func (s *Server) readJSON(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if rerr := checkJSONContentType(r); rerr != nil {
		writeRequestError(w, rerr)
		return nil, false
	}
	return readBody(w, r)
}

// notFoundOrFleetNotFound answers the 404 for a group or a token the store
// didn't find: fleet_not_found when the fleet itself isn't the
// organization's, else code. Asked only on a miss, so a hit costs no extra
// read.
func (s *Server) notFoundOrFleetNotFound(w http.ResponseWriter, r *http.Request, organizationID, fleetID uuid.UUID, code errorCode, message string) {
	if err := s.writeNotFoundOrFleetNotFound(r.Context(), w, organizationID, fleetID, code, message); err != nil {
		s.storeFailed(w, r, storeFleets, err)
	}
}

// writeNotFoundOrFleetNotFound is notFoundOrFleetNotFound reading the fleet
// on ctx (a delete's runs on the delete's own context, deleteContext): when
// the read fails it answers nothing and returns the error for its caller to
// answer.
func (s *Server) writeNotFoundOrFleetNotFound(ctx context.Context, w http.ResponseWriter, organizationID, fleetID uuid.UUID, code errorCode, message string) error {
	_, err := s.fleets.Get(ctx, organizationID, fleetID)
	switch {
	case errors.Is(err, fleets.ErrNotFound):
		writeFleetNotFound(w)
	case err != nil:
		return err
	default:
		writeError(w, http.StatusNotFound, code, message)
	}
	return nil
}

// writeBearerJSON answers a reply that carries a bearer (a fleet's create,
// a rotation, a new default token, a reveal) with Cache-Control: no-store,
// so no proxy or client cache keeps the credential.
func writeBearerJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, value)
}

// mintFailed answers 500 when a bearer can't be made (the keys destroyed):
// the log names the route, never a key or an id.
func (s *Server) mintFailed(w http.ResponseWriter, r *http.Request) {
	s.logger.Error(messageMintFailed, "route", s.routePattern(r))
	writeError(w, http.StatusInternalServerError, codeInternalError, messageInternalError)
}

func writeFleetNotFound(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, codeFleetNotFound, messageFleetNotFound)
}

// decodeFleetListQuery reads the list's query: status (active or
// archived), limit (1 to 100, default 50) and cursor, each at most once.
func decodeFleetListQuery(rawQuery string) (fleets.ListOptions, *requestError) {
	options := fleets.ListOptions{Limit: defaultListLimit}
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return options, invalid("The query string couldn't be read.")
	}
	rawStatus, rerr := queryValue(query, "status")
	if rerr != nil {
		return options, rerr
	}
	if _, present := query["status"]; present {
		status := fleets.Status(rawStatus)
		if !status.Valid() {
			return options, invalid("status must be active or archived.")
		}
		options.Status = &status
	}
	rawLimit, rerr := queryValue(query, "limit")
	if rerr != nil {
		return options, rerr
	}
	if _, present := query["limit"]; present {
		limit, ok := parseLimit(rawLimit)
		if !ok || limit > fleets.MaxListLimit {
			return options, invalid("limit must be an integer from 1 to %d.", fleets.MaxListLimit)
		}
		options.Limit = limit
	}
	rawCursor, rerr := queryValue(query, "cursor")
	if rerr != nil {
		return options, rerr
	}
	if _, present := query["cursor"]; present {
		cursor, err := fleets.ParseCursor(rawCursor)
		if err != nil {
			return options, invalid("cursor isn't one this API gave.")
		}
		options.After = &cursor
	}
	return options, nil
}
