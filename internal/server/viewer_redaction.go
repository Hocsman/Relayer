package server

import (
	"path/filepath"
	"strconv"
	"strings"
)

// A viewer token is for watching agents, and the command line an agent was
// started with can carry a credential: an --api-key flag, a token in a URL. The
// desktop never sends argv to its interface at all; the web gateway sends it to
// operators, who can already rewrite and restart agents, and — until v0.8.6 —
// sent it to viewers too, through getAgentProfiles and getState. v0.8.4's
// changelog said viewers were given agent configurations "without exposing
// secrets"; a probe token planted in argv came back verbatim.

// profilesForRole returns view unchanged for an operator and masked for a
// viewer: no argument vector and no configuration path. The executable label
// and the argument count remain, which is what the interface shows a viewer.
func profilesForRole(view AgentProfilesView, role UserRole) AgentProfilesView {
	if role == RoleOperator {
		return view
	}
	masked := view
	masked.ConfigPath = ""
	masked.Editable = false
	masked.Profiles = make([]AgentProfile, len(view.Profiles))
	for index, profile := range view.Profiles {
		profile.Argv = nil
		profile.PreserveOnSave = true
		masked.Profiles[index] = profile
	}
	return masked
}

// stateForRole returns state unchanged for an operator and, for a viewer,
// replaces each agent's display command with its executable name.
func stateForRole(state AppState, role UserRole) AppState {
	if role == RoleOperator {
		return state
	}
	masked := state
	masked.Agents = make([]AgentState, len(state.Agents))
	for index, agent := range state.Agents {
		agent.DisplayCommand = executableOnly(agent.DisplayCommand)
		masked.Agents[index] = agent
	}
	return masked
}

// executableOnly reduces a display command — each argument quoted and joined by
// spaces, or a fixed marker for an explicit shell — to its executable's base
// name. Nothing after the first argument survives.
func executableOnly(display string) string {
	display = strings.TrimSpace(display)
	if display == "" || !strings.HasPrefix(display, `"`) {
		// The shell marker, or a shape this does not recognise: neither is
		// an argument vector to reduce, and neither is echoed back.
		if display == "[explicit shell]" {
			return display
		}
		return ""
	}
	first, err := strconv.QuotedPrefix(display)
	if err != nil {
		return ""
	}
	executable, err := strconv.Unquote(first)
	if err != nil {
		return ""
	}
	return filepath.Base(executable)
}
