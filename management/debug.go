package management

import (
	"github.com/telflo/opamp-server/internal/diagnostics"
	"github.com/telflo/opamp-server/middleware"
)

// registerDebug registers the diagnostic routes (package diagnostics: Go's
// runtime profiles and the agent endpoint's admission view) on the API's
// router, behind the management secret like every /api/ route: the agent
// port faces the internet, this one never should. The conformance test
// names them (debugRoutes), since api/openapi.yaml leaves them out.
func (s *Server) registerDebug() {
	s.api.handle(diagnostics.ProfilesPattern, diagnostics.Profiles())
	s.api.handle(diagnostics.AdmissionPattern, diagnostics.Admission(s.admissionSnapshot))
}

// admissionSnapshot is the agent endpoint's gate's state, or a disabled
// gate's when the server was built without one (no agent endpoint in this
// process).
func (s *Server) admissionSnapshot() middleware.AdmissionSnapshot {
	if s.admission == nil {
		return middleware.AdmissionSnapshot{}
	}
	return s.admission()
}
