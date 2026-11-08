package management

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"

	"github.com/telflo/opamp-server/store/organizations"
)

// The organization's limits: the most collectors it may have enrolled, and
// its agent requests a minute per pod. Each is null when unset: no cap, and
// the server's default rate.
const limitsPath = organizationPath + "/limits"

// The bounds the table checks: the columns are integers.
const (
	messageMaxCollectors     = "maxCollectors must be an integer from 0 to 2147483647, or null."
	messageRequestsPerMinute = "requestsPerMinute must be an integer from 1 to 2147483647, or null."
)

func (s *Server) registerLimits() {
	s.api.handle("GET "+limitsPath, getOnly(s.getLimits))
	s.api.handle("PUT "+limitsPath, http.HandlerFunc(s.putLimits))
}

// getLimits is GET …/limits: 200 with the organization's limits, or 404
// organization_not_found.
func (s *Server) getLimits(w http.ResponseWriter, r *http.Request) {
	organizationID, rerr := pathUUID(r, "organizationId")
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	if !s.requireOrganization(w, r, organizationID) {
		return
	}
	limits, err := s.organizations.Limits(r.Context(), organizationID)
	switch {
	case errors.Is(err, organizations.ErrNotFound):
		writeOrganizationNotFound(w)
	case err != nil:
		s.storeFailed(w, r, storeOrganizations, err)
	default:
		writeJSON(w, http.StatusOK, limitsReply(limits))
	}
}

// putLimits is PUT …/limits with {"maxCollectors", "requestsPerMinute"},
// both required, either null (unset): 200 with the limits stored; 400; 404
// organization_not_found. A cap below the collectors already enrolled
// removes none of them: it refuses the next enrollment.
//
// When each takes effect: the agent side reads requestsPerMinute through
// the limits cache (middleware.LimitsCacheTTL, 10 s), so a new rate reaches
// each pod within 10 s, not at once; maxCollectors is read by each
// enrollment in its own transaction (under the organization's lock), so a
// new cap, 0 included, holds from the next enrollment on.
func (s *Server) putLimits(w http.ResponseWriter, r *http.Request) {
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
	limits, rerr := decodeLimits(body)
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	stored, err := s.organizations.SetLimits(r.Context(), organizationID, limits)
	switch {
	case errors.Is(err, organizations.ErrNotFound):
		writeOrganizationNotFound(w)
	case errors.Is(err, organizations.ErrInvalidLimits):
		writeRequestError(w, invalid(messageUnstorable))
	case err != nil:
		s.storeFailed(w, r, storeOrganizations, err)
	default:
		writeJSON(w, http.StatusOK, limitsReply(stored))
	}
}

// decodeLimits reads PUT …/limits's body.
func decodeLimits(body []byte) (organizations.Limits, *requestError) {
	var limits organizations.Limits
	raw, rerr := decodeBody(body)
	if rerr != nil {
		return limits, rerr
	}
	top, rerr := decodeObject(raw, "", "", "maxCollectors", "requestsPerMinute")
	if rerr != nil {
		return limits, rerr
	}
	var ok bool
	if limits.MaxCollectors, ok = optionalInt32(top, "maxCollectors", 0); !ok {
		return limits, invalid(messageMaxCollectors)
	}
	if limits.RequestsPerMinute, ok = optionalInt32(top, "requestsPerMinute", 1); !ok {
		return limits, invalid(messageRequestsPerMinute)
	}
	return limits, nil
}

// optionalInt32 reads a nullable integer from min to the most an int32
// holds: any JSON number with a whole value, as JSON Schema's integer has it.
func optionalInt32(top *jsonObject, field string, min float64) (*int32, bool) {
	if top.isNull(field) {
		return nil, true
	}
	var value float64
	if json.Unmarshal(top.fields[field], &value) != nil || value != math.Trunc(value) || value < min || value > math.MaxInt32 {
		return nil, false
	}
	v := int32(value)
	return &v, true
}

type limitsJSON struct {
	MaxCollectors     *int32 `json:"maxCollectors"`
	RequestsPerMinute *int32 `json:"requestsPerMinute"`
}

func limitsReply(l organizations.Limits) limitsJSON {
	return limitsJSON{MaxCollectors: l.MaxCollectors, RequestsPerMinute: l.RequestsPerMinute}
}
