package daemon

import (
	"github.com/msageha/maestro_v2/internal/daemon/paneactivity"
)

// observePaneActivityForAgent is a test-only legacy boolean wrapper around
// observePaneVerdictForAgent. Production code uses observePaneVerdictForAgent
// directly so VerdictUncertain is distinguished from VerdictIdle; tests that
// only need the binary "alive vs. unknown" judgment use this helper.
func (qh *QueueHandler) observePaneActivityForAgent(agentID string) bool {
	return qh.observePaneVerdictForAgent(agentID) == paneactivity.VerdictActive
}
