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
