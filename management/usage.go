package management

import (
	"net/http"
)

// What uses a configuration's published versions: for the platform's
// refusal to delete a configuration that a collector group or a collector
// still uses, which the publication deletes refuse anyway (409
// publication_in_use).
const usagePath = apiPrefix + "v1/organizations/{organizationId}/configurations/{configurationId}/usage"

func (s *Server) registerUsage() {
	s.api.handle("GET "+usagePath, getOnly(s.getUsage))
}

// getUsage is GET …/configurations/{configurationId}/usage: 200 with each
// of the configuration's published versions, newest first, the collector
// groups assigned it and how many collectors are pinned to it; none for a
// configuration with no published version. 404 organization_not_found.
func (s *Server) getUsage(w http.ResponseWriter, r *http.Request) {
	organizationID, rerr := pathUUID(r, "organizationId")
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	configurationID, rerr := pathUUID(r, "configurationId")
	if rerr != nil {
		writeRequestError(w, rerr)
		return
	}
	if !s.requireOrganization(w, r, organizationID) {
		return
	}
	usage, err := s.publications.Usage(r.Context(), organizationID, configurationID)
	if err != nil {
		s.storeFailed(w, r, storePublications, err)
		return
	}
	reply := usageReply{ConfigurationID: configurationID.String(), Publications: make([]publicationUsageReply, 0, len(usage))}
	for _, u := range usage {
		p := u.Publication
		groups := make([]groupRefReply, 0, len(u.Groups))
		for _, g := range u.Groups {
			groups = append(groups, groupRefReply{FleetID: g.FleetID.String(), GroupID: g.GroupID.String()})
		}
		reply.InUse = reply.InUse || u.InUse()
		reply.Publications = append(reply.Publications, publicationUsageReply{
			ID:               p.ID.String(),
			Version:          versionReply{ID: p.VersionID.String(), Number: p.VersionNumber, Label: p.VersionLabel},
			PublishedAt:      formatTime(p.PublishedAt),
			Groups:           groups,
			PinnedCollectors: u.PinnedCollectors,
		})
	}
	writeJSON(w, http.StatusOK, reply)
}

// usageReply is openapi.yaml's ConfigurationUsage.
type usageReply struct {
	ConfigurationID string                  `json:"configurationId"`
	InUse           bool                    `json:"inUse"`
	Publications    []publicationUsageReply `json:"publications"`
}

type publicationUsageReply struct {
	ID               string          `json:"id"`
	Version          versionReply    `json:"version"`
	PublishedAt      string          `json:"publishedAt"`
	Groups           []groupRefReply `json:"groups"`
	PinnedCollectors int             `json:"pinnedCollectors"`
}

type groupRefReply struct {
	FleetID string `json:"fleetId"`
	GroupID string `json:"groupId"`
}
