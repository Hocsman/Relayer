package server

import (
	"errors"
	"fmt"
	"strings"
)

// A viewer token is a token to read the terminals: an agent prints what it
// likes, a secret it echoes included, and the snapshots carry the screen as
// it is. A gateway whose viewers should see only the prompts and the run's
// state is started with --viewer-terminals hidden.

// errViewerTerminals refuses a viewer a recording's contents when terminals
// are hidden from viewers: a recording is the terminal, frame by frame.
var errViewerTerminals = errors.New("this gateway does not show terminals to viewers")

// parseViewerTerminals reads --viewer-terminals and reports whether terminals
// are hidden from viewers.
func parseViewerTerminals(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "shown":
		return false, nil
	case "hidden":
		return true, nil
	default:
		return false, fmt.Errorf("--viewer-terminals must be shown or hidden, not %q", value)
	}
}

func (gh *gatewayHandler) terminalsHiddenFrom(role UserRole) bool {
	return gh.hideViewerTerminals && role != RoleOperator
}

// stateFor is the state a client of the role receives: stateForRole's masking,
// and no agent's output for a viewer from whom terminals are hidden.
func (gh *gatewayHandler) stateFor(role UserRole) AppState {
	state := stateForRole(gh.ctrl.GetState(), role)
	if !gh.terminalsHiddenFrom(role) {
		return state
	}
	agents := make([]AgentState, len(state.Agents))
	for index, agent := range state.Agents {
		agent.Output = ""
		agents[index] = agent
	}
	state.Agents = agents
	return state
}
