package daemon

import "github.com/msageha/maestro_v2/internal/tmux"

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
