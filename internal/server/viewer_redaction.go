package server

import "errors"

// A viewer token is for watching agents, and the command line an agent was
// started with can carry a credential: an --api-key flag, a token in a URL. The
// desktop never sends argv to its interface at all; the web gateway sends it to
// operators, who can already rewrite and restart agents, and — until v0.8.6 —
// sent it to viewers too, through getAgentProfiles. v0.8.4's changelog said
// viewers were given agent configurations "without exposing secrets"; a probe
// token planted in argv came back verbatim.

// errViewerProfiles replaces a profile-loading error for a viewer: the
// loader's error names the configuration file's path.
var errViewerProfiles = errors.New("the agent profiles could not be loaded")

// profilesForRole returns view unchanged for an operator and masked for a
// viewer: no argument vector and no configuration path. The executable label
// and the argument count remain, which is what the interface shows a viewer.
func profilesForRole(view AgentProfilesView, role UserRole) AgentProfilesView {
	if role == RoleOperator {
		return view
	}
	masked := view
	masked.ConfigPath = ""
	masked.Profiles = make([]AgentProfile, len(view.Profiles))
	for index, profile := range view.Profiles {
		profile.Argv = nil
		profile.PreserveOnSave = true
		masked.Profiles[index] = profile
	}
	return masked
}

// stateForRole returns state unchanged for an operator. For a viewer it drops
// the startup notices and the audit journal's path: the notices are operator
// diagnostics and name the configuration and journal files. Each agent's
// display command is already only its executable's name.
func stateForRole(state AppState, role UserRole) AppState {
	if role == RoleOperator {
		return state
	}
	masked := state
	masked.Notices = nil
	masked.Audit.Path = ""
	return masked
}

// errViewerAudit replaces an audit error for a viewer: the error from opening
// the journal names its path.
var errViewerAudit = errors.New("the audit journal could not be read")

// auditErrorForRole returns err unchanged for an operator and a fixed message
// for a viewer.
func auditErrorForRole(err error, role UserRole) error {
	if err == nil || role == RoleOperator {
		return err
	}
	return errViewerAudit
}

// auditPathForRole is the journal's path for an operator and nothing for a
// viewer. getState already hid it from viewers; the audit summary and the
// journal check still sent it.
func auditPathForRole(path string, role UserRole) string {
	if role == RoleOperator {
		return path
	}
	return ""
}
