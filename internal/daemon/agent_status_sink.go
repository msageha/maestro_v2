package daemon

import "github.com/msageha/maestro_v2/internal/tmux"

// AgentStatusSink abstracts per-pane status updates so the daemon can
// signal "this agent is no longer working" without depending on a
// concrete tmux backend. Tests inject a recording stub; production
// wires tmuxAgentStatusSink which writes the @status user variable.
type AgentStatusSink interface {
	SetIdle(agentID string, logFn logFunc)
}

type tmuxAgentStatusSink struct{}

func (tmuxAgentStatusSink) SetIdle(agentID string, logFn logFunc) {
	paneTarget, err := tmux.FindPaneByAgentID(agentID)
	if err != nil {
		logFn(LogLevelDebug, "set_agent_idle pane_not_found agent=%s: %v", agentID, err)
		return
	}
	if err := tmux.SetUserVar(paneTarget, "status", "idle"); err != nil {
		logFn(LogLevelWarn, "set_agent_idle_failed agent=%s: %v", agentID, err)
	}
}
