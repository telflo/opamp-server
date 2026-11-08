package serve

// Role is which components a serve process runs. The zero value runs both.
type Role int

const (
	// RoleAll runs the agent endpoint and the management API in one
	// process: `serve`.
	RoleAll Role = iota
	// RoleAgent runs only the agent endpoint: `serve agent`. It reads no
	// management secret.
	RoleAgent
	// RoleManagement runs only the management API: `serve management`. It
	// reads no agent setting.
	RoleManagement
)

// ParseRole is the role `serve`'s argument names: none for RoleAll,
// "agent" or "management"; anything else is not a role.
func ParseRole(args []string) (Role, bool) {
	switch {
	case len(args) == 0:
		return RoleAll, true
	case len(args) > 1:
		return 0, false
	case args[0] == "agent":
		return RoleAgent, true
	case args[0] == "management":
		return RoleManagement, true
	}
	return 0, false
}

// String is the role as its log line names it.
func (r Role) String() string {
	switch r {
	case RoleAgent:
		return "agent"
	case RoleManagement:
		return "management"
	}
	return "all"
}

// runsAgent reports whether the role runs the agent endpoint.
func (r Role) runsAgent() bool { return r != RoleManagement }

// runsManagement reports whether the role runs the management API.
func (r Role) runsManagement() bool { return r != RoleAgent }
