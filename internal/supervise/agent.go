package supervise

// AgentSpec is the fixed identity of one supervised agent for a run: what the
// journal names it by. It carries no command line, environment or output.
type AgentSpec struct {
	SessionID string
	AgentID   string
	Name      string
	Backend   string
	Adapter   string
}

// Agent is what the core knows of one agent's process: the state that decides
// whether a prompt is taken in, a decision delivered or a line sent. Its
// output and the rest of what a front end displays are the front end's own.
type Agent struct {
	AgentSpec
	Status      string
	Running     bool
	Attached    bool
	InputFrozen bool
	ExitCode    *int
}

func cloneExitCode(value *int) *int {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
