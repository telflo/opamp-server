package management

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/telflo/opamp-server/store/collectors"
)

// The collector routes: a fleet's collectors (openapi.yaml's
// CollectorInstance), listed, searched, sorted and paged; one read,
// renamed, moved, pinned or unpinned, or deleted; many pinned at once; and
// a collector's error history. Each names the fleet, and a collector of
// another fleet, even the organization's, is the same 404 as one that
// doesn't exist. Liveness and sync come from the server's clock. A
// collector's reported attributes and its effective configuration's error
// text are answered here and nowhere else, and never logged. The literal
// pin path is registered before the collector id's paths.
const (
	collectorsPath      = fleetPath + "/collectors"
	collectorsPinPath   = collectorsPath + "/pin"
	collectorPath       = collectorsPath + "/{collectorId}"
	collectorErrorsPath = collectorPath + "/errors"
)

// The collector routes' fixed messages.
const (
	messageCollectorNotFound  = "The fleet has no collector with this id."
	messageCollectorNameTaken = "Another of the organization's collectors has this name."
	messageCollectorName      = "name must be 1 to 200 characters, with no leading or trailing whitespace and no control character."
	messageNoCollectorChange  = "The request body must name name, groupId, pin or pinnedVariableGroupId."
	messageNoBulkPinChange    = "The request body must name pin or pinnedVariableGroupId."
	messagePin                = `pin must be {"kind": "publication", "id": a UUID}, or null.`
	messageCollectorIDs       = "collectorIds must be an array of 1 to 500 UUIDs."
	messageCursor             = "cursor isn't one this API gave."
)

func (s *Server) registerCollectors() {
	s.api.handle("GET "+collectorsPath, getOnly(s.listCollectors))
	s.api.handle("POST "+collectorsPinPath, http.HandlerFunc(s.bulkPinCollectors))
	s.api.handle("GET "+collectorPath, getOnly(s.getCollector))
	s.api.handle("PATCH "+collectorPath, http.HandlerFunc(s.updateCollector))
	s.api.handle("DELETE "+collectorPath, http.HandlerFunc(s.deleteCollector))
	s.api.handle("GET "+collectorErrorsPath, getOnly(s.listCollectorErrors))
}

// listCollectors is GET …/collectors?q=…&groupId=…&sort=…&order=…&limit=…&cursor=…:
// 200 with a page of the fleet's collectors; 400; 404 fleet_not_found.
func (s *Server) listCollectors(w http.ResponseWriter, r *http.Request) {
	organizationID, fleetID, ok := s.organizationAndFleet(w, r)
	if !ok {
		return
	}
	options, rerr := decodeCollectorListQuery(r.URL.RawQuery)
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	page, err := s.collectors.List(r.Context(), organizationID, fleetID, options)
	switch {
	case errors.Is(err, collectors.ErrFleetNotFound):
		writeFleetNotFound(w)
		return
	case errors.Is(err, collectors.ErrInvalidCursor):
		// A cursor of another sort or order.
		writeRequestError(w, invalid(messageCursor))
		return
	case errors.Is(err, collectors.ErrInvalid):
		writeRequestError(w, invalid(messageUnstorable))
		return
	case err != nil:
		s.storeFailed(w, r, storeCollectors, err)
		return
	}
	now := s.clock()
	reply := collectorListReply{Collectors: make([]collectorInstanceReply, 0, len(page.Collectors))}
	for _, l := range page.Collectors {
		reply.Collectors = append(reply.Collectors, collectorInstanceJSON(l, now))
	}
	if page.Next != nil {
		next := page.Next.String()
		reply.NextCursor = &next
	}
	writeJSON(w, http.StatusOK, reply)
}

// getCollector is GET …/collectors/{collectorId}: 200 with the collector,
// or 404.
func (s *Server) getCollector(w http.ResponseWriter, r *http.Request) {
	organizationID, fleetID, collectorID, ok := s.organizationFleetAndCollector(w, r)
	if !ok {
		return
	}
	listed, err := s.collectors.Get(r.Context(), organizationID, fleetID, collectorID)
	switch {
	case errors.Is(err, collectors.ErrNotFound):
		s.notFoundOrFleetNotFound(w, r, organizationID, fleetID, codeCollectorNotFound, messageCollectorNotFound)
	case err != nil:
		s.storeFailed(w, r, storeCollectors, err)
	default:
		writeJSON(w, http.StatusOK, collectorInstanceJSON(listed, s.clock()))
	}
}

// updateCollector is PATCH …/collectors/{collectorId} with any of "name",
// "groupId", "pin" (null unpins) and "pinnedVariableGroupId" (null returns
// it to its group's), at least one, applied together or not at all: 200
// with the collector; 400; 404 (the collector, or a group the fleet doesn't
// hold: collector_group_not_found); 409 collector_name_taken; 422
// publication_not_found for a pinned published version the organization
// doesn't hold, variable_group_not_found for a variable group it doesn't
// hold, variables_undefined for a pin to a published version referencing
// a name no variable of the organization has (with the names).
func (s *Server) updateCollector(w http.ResponseWriter, r *http.Request) {
	organizationID, fleetID, collectorID, ok := s.organizationFleetAndCollector(w, r)
	if !ok {
		return
	}
	body, ok := s.readJSON(w, r)
	if !ok {
		return
	}
	changes, rerr := decodeCollectorUpdate(body)
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	listed, err := s.collectors.Update(r.Context(), organizationID, fleetID, collectorID, changes, s.clock())
	switch {
	case errors.Is(err, collectors.ErrNotFound):
		s.notFoundOrFleetNotFound(w, r, organizationID, fleetID, codeCollectorNotFound, messageCollectorNotFound)
	case errors.Is(err, collectors.ErrGroupNotFound):
		writeError(w, http.StatusNotFound, codeCollectorGroupNotFound, messageCollectorGroupNotFound)
	case errors.Is(err, collectors.ErrNameTaken):
		writeError(w, http.StatusConflict, codeCollectorNameTaken, messageCollectorNameTaken)
	case s.writePinRefused(w, err):
	case errors.Is(err, collectors.ErrInvalid):
		writeRequestError(w, invalid(messageUnstorable))
	case err != nil:
		s.storeFailed(w, r, storeCollectors, err)
	default:
		writeJSON(w, http.StatusOK, collectorInstanceJSON(listed, s.clock()))
	}
}

// writePinRefused answers a pin's 422, as a group's assignment is
// answered, and reports whether it answered: a pinned published version
// the organization doesn't hold (publication_not_found), a pinned variable
// group it doesn't hold (variable_group_not_found), or a pinned published
// version referencing names no variable of the organization has
// (variables_undefined, with the names).
func (s *Server) writePinRefused(w http.ResponseWriter, err error) bool {
	var undefined *collectors.UndefinedError
	switch {
	case errors.Is(err, collectors.ErrPublicationNotFound):
		writeError(w, http.StatusUnprocessableEntity, codePublicationNotFound, messagePublicationNotFound)
	case errors.Is(err, collectors.ErrVariableGroupNotFound):
		writeVariableGroupTargetNotFound(w)
	case errors.As(err, &undefined):
		writeVariablesUndefined(w, undefined.Undefined)
	default:
		return false
	}
	return true
}

// deleteCollector is DELETE …/collectors/{collectorId}: 204, the collector
// and its error history gone (on its next poll it is asked for its full
// state and enrolls again with the answer; its slot under the cap is free
// meanwhile), or 404.
func (s *Server) deleteCollector(w http.ResponseWriter, r *http.Request) {
	organizationID, fleetID, collectorID, ok := s.organizationFleetAndCollector(w, r)
	if !ok {
		return
	}
	deleted, err := s.collectors.Delete(r.Context(), organizationID, fleetID, collectorID)
	switch {
	case err != nil:
		s.storeFailed(w, r, storeCollectors, err)
	case !deleted:
		s.notFoundOrFleetNotFound(w, r, organizationID, fleetID, codeCollectorNotFound, messageCollectorNotFound)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// bulkPinCollectors is POST …/collectors/pin with {"collectorIds", "pin"?,
// "pinnedVariableGroupId"?}, at least one of the last two: an absent one
// leaves each collector's as it is; pin null unpins, and
// pinnedVariableGroupId null returns each to its group's variable group.
// 200 with the ids changed as asked and those the fleet doesn't hold, each
// sorted and once, in one transaction; 400; 404 fleet_not_found; 422
// publication_not_found, variable_group_not_found or variables_undefined.
func (s *Server) bulkPinCollectors(w http.ResponseWriter, r *http.Request) {
	organizationID, fleetID, ok := s.organizationAndFleet(w, r)
	if !ok {
		return
	}
	body, ok := s.readJSON(w, r)
	if !ok {
		return
	}
	ids, pin, vg, rerr := decodeBulkPin(body)
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	result, err := s.collectors.BulkPin(r.Context(), organizationID, fleetID, ids, pin, vg, s.clock())
	switch {
	case errors.Is(err, collectors.ErrFleetNotFound):
		writeFleetNotFound(w)
	case s.writePinRefused(w, err):
	case errors.Is(err, collectors.ErrInvalid):
		writeRequestError(w, invalid(messageUnstorable))
	case err != nil:
		s.storeFailed(w, r, storeCollectors, err)
	default:
		writeJSON(w, http.StatusOK, bulkPinReply{Updated: idStrings(result.Updated), NotFound: idStrings(result.NotFound)})
	}
}

// listCollectorErrors is GET …/collectors/{collectorId}/errors?limit=…&cursor=…:
// 200 with a page of the collector's error history, newest first; 400; 404.
func (s *Server) listCollectorErrors(w http.ResponseWriter, r *http.Request) {
	organizationID, fleetID, collectorID, ok := s.organizationFleetAndCollector(w, r)
	if !ok {
		return
	}
	query, rerr := parseQuery(r.URL.RawQuery, "limit", "cursor")
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	limit, rerr := listLimit(query, collectors.MaxListLimit)
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	var after *collectors.ErrorCursor
	if raw, present := query["cursor"]; present {
		cursor, err := collectors.ParseErrorCursor(raw)
		if err != nil {
			writeRequestError(w, invalid(messageCursor))
			return
		}
		after = &cursor
	}
	page, err := s.collectors.ListErrors(r.Context(), organizationID, fleetID, collectorID, limit, after)
	switch {
	case errors.Is(err, collectors.ErrNotFound):
		s.notFoundOrFleetNotFound(w, r, organizationID, fleetID, codeCollectorNotFound, messageCollectorNotFound)
		return
	case err != nil:
		s.storeFailed(w, r, storeCollectors, err)
		return
	}
	reply := collectorErrorListReply{Errors: make([]collectorErrorReply, 0, len(page.Errors))}
	for _, e := range page.Errors {
		reply.Errors = append(reply.Errors, collectorErrorReply{
			ID: e.ID.String(), ReportedAt: formatTime(e.ReportedAt), ConfigSHA256: e.ConfigSHA256, Message: e.Message,
		})
	}
	if page.Next != nil {
		next := page.Next.String()
		reply.NextCursor = &next
	}
	writeJSON(w, http.StatusOK, reply)
}

// organizationFleetAndCollector reads the three path ids (400), then
// requires the organization (404 organization_not_found). On failure it has
// answered.
func (s *Server) organizationFleetAndCollector(w http.ResponseWriter, r *http.Request) (organizationID, fleetID, collectorID uuid.UUID, ok bool) {
	ids, ok := s.underOrganization(w, r, "fleetId", "collectorId")
	if !ok {
		return uuid.Nil, uuid.Nil, uuid.Nil, false
	}
	return ids[0], ids[1], ids[2], true
}

// parseQuery reads a query string whose parameters are among known, each
// given at most once (a repeated one is a 400; an unknown one is ignored,
// as the other lists ignore one). It answers each present parameter's
// value.
func parseQuery(rawQuery string, known ...string) (map[string]string, *requestError) {
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return nil, invalid("The query string couldn't be read.")
	}
	values := map[string]string{}
	for _, name := range known {
		value, rerr := queryValue(query, name)
		if rerr != nil {
			return nil, rerr
		}
		if _, present := query[name]; present {
			values[name] = value
		}
	}
	return values, nil
}

// listLimit reads a list's limit: 1 to max, default 50.
func listLimit(query map[string]string, max int) (int, *requestError) {
	raw, present := query["limit"]
	if !present {
		return defaultListLimit, nil
	}
	limit, ok := parseLimit(raw)
	if !ok || limit > max {
		return 0, invalid("limit must be an integer from 1 to %d.", max)
	}
	return limit, nil
}

// decodeCollectorListQuery reads the collector list's query: q (at most 100
// characters), groupId, sort (created, lastSeen or name; default created),
// order (asc or desc; default desc), limit (1 to 100, default 50) and
// cursor, each at most once.
func decodeCollectorListQuery(rawQuery string) (collectors.ListOptions, *requestError) {
	options := collectors.ListOptions{Sort: collectors.SortCreated, Order: collectors.Descending}
	query, rerr := parseQuery(rawQuery, "q", "groupId", "sort", "order", "limit", "cursor")
	if rerr != nil {
		return options, rerr
	}
	if q, present := query["q"]; present {
		if !utf8.ValidString(q) || strings.ContainsRune(q, 0) || utf8.RuneCountInString(q) > collectors.MaxQueryLength {
			return options, invalid("q must be at most %d characters, with no U+0000.", collectors.MaxQueryLength)
		}
		options.Q = q
	}
	if raw, present := query["groupId"]; present {
		id, ok := parseUUID(raw)
		if !ok {
			return options, invalid("groupId must be a UUID.")
		}
		options.GroupID = &id
	}
	if raw, present := query["sort"]; present {
		switch sort := collectors.Sort(raw); sort {
		case collectors.SortCreated, collectors.SortLastSeen, collectors.SortName:
			options.Sort = sort
		default:
			return options, invalid("sort must be created, lastSeen or name.")
		}
	}
	if raw, present := query["order"]; present {
		switch order := collectors.Order(raw); order {
		case collectors.Ascending, collectors.Descending:
			options.Order = order
		default:
			return options, invalid("order must be asc or desc.")
		}
	}
	limit, rerr := listLimit(query, collectors.MaxListLimit)
	if rerr != nil {
		return options, rerr
	}
	options.Limit = limit
	if raw, present := query["cursor"]; present {
		cursor, err := collectors.ParseCursor(raw)
		if err != nil {
			return options, invalid(messageCursor)
		}
		options.After = &cursor
	}
	return options, nil
}

// decodeCollectorUpdate reads PATCH …/collectors/{collectorId}'s body: any
// of "name" and "groupId" (neither null), "pin" (null unpins) and
// "pinnedVariableGroupId" (null clears it), at least one.
func decodeCollectorUpdate(body []byte) (collectors.Changes, *requestError) {
	var c collectors.Changes
	raw, rerr := decodeBody(body)
	if rerr != nil {
		return c, rerr
	}
	top, rerr := decodeObjectWithOptional(raw, "", "", nil, []string{"name", "groupId", "pin", "pinnedVariableGroupId"})
	if rerr != nil {
		return c, rerr
	}
	if len(top.fields) == 0 {
		return c, invalid(messageNoCollectorChange)
	}
	if top.has("name") {
		name, rerr := top.string("name")
		if rerr != nil || !collectors.ValidName(name) {
			return c, invalid(messageCollectorName)
		}
		c.Name = &name
	}
	if top.has("groupId") {
		id, rerr := top.uuid("groupId")
		if rerr != nil {
			return c, rerr
		}
		c.GroupID = &id
	}
	if top.has("pin") {
		ref, rerr := decodePin(top)
		if rerr != nil {
			return c, rerr
		}
		c.Pin = &collectors.PinChange{Ref: ref}
	}
	if top.has("pinnedVariableGroupId") {
		if c.PinnedVariableGroup, rerr = decodePinnedVariableGroup(top); rerr != nil {
			return c, rerr
		}
	}
	return c, nil
}

// decodeBulkPin reads POST …/collectors/pin's body: {"collectorIds": 1 to
// 500 UUIDs, "pin"?: {kind, id} or null, "pinnedVariableGroupId"?: a UUID
// or null}, at least one of the last two. Each is answered as the change
// BulkPin makes: nil when absent (leave each as it is), a change with no
// target for null (unpin; return to the group's variable group).
func decodeBulkPin(body []byte) ([]uuid.UUID, *collectors.PinChange, *collectors.VariableGroupChange, *requestError) {
	raw, rerr := decodeBody(body)
	if rerr != nil {
		return nil, nil, nil, rerr
	}
	top, rerr := decodeObjectWithOptional(raw, "", "", []string{"collectorIds"}, []string{"pin", "pinnedVariableGroupId"})
	if rerr != nil {
		return nil, nil, nil, rerr
	}
	var items []json.RawMessage
	if top.isNull("collectorIds") || json.Unmarshal(top.fields["collectorIds"], &items) != nil ||
		len(items) < 1 || len(items) > collectors.MaxBulkPin {
		return nil, nil, nil, invalid(messageCollectorIDs)
	}
	ids := make([]uuid.UUID, 0, len(items))
	for _, item := range items {
		var value string
		if json.Unmarshal(item, &value) != nil {
			return nil, nil, nil, invalid(messageCollectorIDs)
		}
		id, ok := parseUUID(value)
		if !ok {
			return nil, nil, nil, invalid(messageCollectorIDs)
		}
		ids = append(ids, id)
	}
	if !top.has("pin") && !top.has("pinnedVariableGroupId") {
		return nil, nil, nil, invalid(messageNoBulkPinChange)
	}
	var pin *collectors.PinChange
	if top.has("pin") {
		ref, rerr := decodePin(top)
		if rerr != nil {
			return nil, nil, nil, rerr
		}
		pin = &collectors.PinChange{Ref: ref}
	}
	var vg *collectors.VariableGroupChange
	if top.has("pinnedVariableGroupId") {
		if vg, rerr = decodePinnedVariableGroup(top); rerr != nil {
			return nil, nil, nil, rerr
		}
	}
	return ids, pin, vg, nil
}

// decodePinnedVariableGroup reads a present "pinnedVariableGroupId": a UUID,
// or null for none (the collector's group's then applies).
func decodePinnedVariableGroup(top *jsonObject) (*collectors.VariableGroupChange, *requestError) {
	id, rerr := optionalUUID(top, "pinnedVariableGroupId", messagePinnedVariableGroupID)
	if rerr != nil {
		return nil, rerr
	}
	return &collectors.VariableGroupChange{ID: id}, nil
}

// decodePin reads a "pin": null (none), or {"kind": "publication", "id"}.
// Any other kind ("import" among them) is refused with messagePin.
func decodePin(top *jsonObject) (*collectors.Ref, *requestError) {
	if top.isNull("pin") {
		return nil, nil
	}
	pin, rerr := top.object("pin", "kind", "id")
	if rerr != nil {
		return nil, invalid(messagePin)
	}
	kind, rerr := pin.string("kind")
	if rerr != nil {
		return nil, invalid(messagePin)
	}
	if kind != string(collectors.SourcePublication) {
		return nil, invalid(messagePin)
	}
	id, rerr := pin.uuid("id")
	if rerr != nil {
		return nil, invalid(messagePin)
	}
	return &collectors.Ref{Source: collectors.SourcePublication, ID: id}, nil
}

// collectorInstanceReply is a collector as the API answers it (openapi.yaml's
// CollectorInstance): what it is, its pin, what resolves for it, what it was
// served, what it reports running, its reported fields, and its liveness and
// sync at the server's clock.
type collectorInstanceReply struct {
	ID          string    `json:"id"`
	InstanceUID string    `json:"instanceUid"`
	Name        string    `json:"name"`
	FleetID     string    `json:"fleetId"`
	GroupID     string    `json:"groupId"`
	Pin         *pinReply `json:"pin"`
	// PinnedVariableGroupID is a variable group pinned on it, independent
	// of Pin, or null.
	PinnedVariableGroupID *string                   `json:"pinnedVariableGroupId"`
	Resolution            *resolutionReply          `json:"resolution"`
	Served                *servedReply              `json:"served"`
	Effective             *effectiveReply           `json:"effective"`
	Reported              collectorInstanceReported `json:"reported"`
	EnrollmentTokenID     *string                   `json:"enrollmentTokenId"`
	LastSeenAt            string                    `json:"lastSeenAt"`
	Liveness              collectors.Liveness       `json:"liveness"`
	Sync                  string                    `json:"sync"`
	CreatedAt             string                    `json:"createdAt"`
	UpdatedAt             string                    `json:"updatedAt"`
}

type pinReply struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type resolutionReply struct {
	From   string `json:"from"`
	ID     string `json:"id"`
	SHA256 string `json:"sha256"`
}

type servedReply struct {
	Kind   *string `json:"kind"`
	ID     *string `json:"id"`
	SHA256 string  `json:"sha256"`
	At     *string `json:"at"`
}

type effectiveReply struct {
	SHA256 string  `json:"sha256"`
	Status string  `json:"status"`
	Error  *string `json:"error"`
	At     string  `json:"at"`
}

type collectorInstanceReported struct {
	CollectorVersion *string           `json:"collectorVersion"`
	IP               *string           `json:"ip"`
	HostIP           *string           `json:"hostIp"`
	Attributes       map[string]string `json:"attributes"`
}

// resolutionFrom is how the API names where a resolution came from.
var resolutionFrom = map[collectors.From]string{
	collectors.FromPinnedPublication: "pinnedPublication",
	collectors.FromGroupPublication:  "groupPublication",
}

// syncName is the API's name for each sync value.
var syncName = map[collectors.Sync]string{
	collectors.InSync:     "inSync",
	collectors.Pending:    "pending",
	collectors.Failed:     "failed",
	collectors.Drifted:    "drifted",
	collectors.Unassigned: "unassigned",
}

func collectorInstanceJSON(l collectors.Listed, now time.Time) collectorInstanceReply {
	c := l.Collector
	reply := collectorInstanceReply{
		ID: c.ID.String(), InstanceUID: c.InstanceUID.String(), Name: c.Name,
		FleetID: c.FleetID.String(), GroupID: c.GroupID.String(),
		Reported: collectorInstanceReported{
			CollectorVersion: c.ReportedCollectorVersion, IP: c.ReportedIP, HostIP: c.ReportedHostIP,
			Attributes: c.ReportedAttributes,
		},
		PinnedVariableGroupID: optionalIDString(c.PinnedVariableGroupID),
		LastSeenAt:            formatTime(c.LastSeenAt),
		Liveness:              collectors.LivenessAt(c.LastSeenAt, now),
		Sync:                  syncName[l.Sync()],
		CreatedAt:             formatTime(c.CreatedAt), UpdatedAt: formatTime(c.UpdatedAt),
	}
	if c.Pinned != nil {
		reply.Pin = &pinReply{Kind: string(c.Pinned.Source), ID: c.Pinned.ID.String()}
	}
	if from, resolved := resolutionFrom[l.Resolution.From]; resolved {
		reply.Resolution = &resolutionReply{From: from, ID: l.Resolution.ID.String(), SHA256: l.Resolution.SHA256}
	}
	if c.ServedSHA256 != nil {
		served := &servedReply{SHA256: *c.ServedSHA256, At: formatOptionalTime(c.ServedAt)}
		if c.Served != nil {
			kind, id := string(c.Served.Source), c.Served.ID.String()
			served.Kind, served.ID = &kind, &id
		}
		reply.Served = served
	}
	if c.EffectiveSHA256 != nil && c.EffectiveStatus != nil && c.EffectiveAt != nil {
		reply.Effective = &effectiveReply{
			SHA256: *c.EffectiveSHA256, Status: string(*c.EffectiveStatus), Error: c.EffectiveError, At: formatTime(*c.EffectiveAt),
		}
	}
	if c.EnrollmentTokenID != nil {
		id := c.EnrollmentTokenID.String()
		reply.EnrollmentTokenID = &id
	}
	return reply
}

type collectorListReply struct {
	Collectors []collectorInstanceReply `json:"collectors"`
	NextCursor *string                  `json:"nextCursor"`
}

type bulkPinReply struct {
	Updated  []string `json:"updated"`
	NotFound []string `json:"notFound"`
}

type collectorErrorReply struct {
	ID           string  `json:"id"`
	ReportedAt   string  `json:"reportedAt"`
	ConfigSHA256 *string `json:"configSha256"`
	Message      string  `json:"message"`
}

type collectorErrorListReply struct {
	Errors     []collectorErrorReply `json:"errors"`
	NextCursor *string               `json:"nextCursor"`
}

// idStrings is ids as the API writes them, never null.
func idStrings(ids []uuid.UUID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	return out
}
