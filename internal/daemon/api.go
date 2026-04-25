package daemon

import (
	"github.com/msageha/maestro_v2/internal/uds"
)

// API groups all UDS request handler methods for the daemon.
// It acts as a facade, delegating to domain-specific handler structs.
type API struct {
	shared    *apiContext
	result    *ResultWriteAPI
	queue     *QueueWriteAPI
	plan      *PlanAPI
	heartbeat *HeartbeatAPI
	dashboard *DashboardAPI
	skill     *SkillAPI
}

// systemHandlers holds UDS handlers that require direct Daemon access.
type systemHandlers struct {
	scan     func(*uds.Request) *uds.Response
	shutdown func(*uds.Request) *uds.Response
}

// registerHandlers registers UDS request handlers on the given server.
func (a *API) registerHandlers(server *uds.Server, sysHandlers systemHandlers) {
	server.Handle("ping", func(_ *uds.Request) *uds.Response {
		return uds.SuccessResponse(map[string]string{"status": "ok"})
	})
	server.Handle("scan", sysHandlers.scan)
	server.Handle("shutdown", sysHandlers.shutdown)

	server.Handle("queue_write", a.handleQueueWrite)
	server.Handle("result_write", a.handleResultWrite)
	server.Handle("task_heartbeat", a.handleTaskHeartbeat)
	server.Handle("plan", a.handlePlan)
	server.Handle("dashboard", a.handleDashboard)
	server.Handle("skill_approve", a.handleSkillApprove)
	server.Handle("skill_reject", a.handleSkillReject)
}

// --- Delegation methods (preserve backward compatibility for tests) ---

func (a *API) handleQueueWrite(req *uds.Request) *uds.Response {
	return a.queue.handleQueueWrite(req)
}

func (a *API) handleResultWrite(req *uds.Request) *uds.Response {
	return a.result.handleResultWrite(req)
}

func (a *API) handleTaskHeartbeat(req *uds.Request) *uds.Response {
	return a.heartbeat.handleTaskHeartbeat(req)
}

func (a *API) handlePlan(req *uds.Request) *uds.Response {
	return a.plan.handlePlan(req)
}

func (a *API) handleDashboard(req *uds.Request) *uds.Response {
	return a.dashboard.handleDashboard(req)
}

func (a *API) handleSkillApprove(req *uds.Request) *uds.Response {
	return a.skill.handleSkillApprove(req)
}

func (a *API) handleSkillReject(req *uds.Request) *uds.Response {
	return a.skill.handleSkillReject(req)
}

// notifySelfWrite delegates to the shared apiContext.
//
//nolint:unused // exercised from internal/daemon test files (golangci-lint runs with tests:false)
func (a *API) notifySelfWrite(queuePath, writeType string, data any) {
	a.shared.notifySelfWrite(queuePath, writeType, data)
}

// recordSelfWrite delegates to the shared apiContext.
//
//nolint:unused // exercised from internal/daemon test files (golangci-lint runs with tests:false)
func (a *API) recordSelfWrite(path string, data any) {
	a.shared.recordSelfWrite(path, data)
}

// publishQueueWritten delegates to the shared apiContext.
//
//nolint:unused // exercised from internal/daemon test files (golangci-lint runs with tests:false)
func (a *API) publishQueueWritten(source string) {
	a.shared.publishQueueWritten(source)
}

// writeLearnings delegates to the result write handler.
//
//nolint:unused // exercised from internal/daemon/learnings_test.go (golangci-lint runs with tests:false)
func (a *API) writeLearnings(params ResultWriteParams, resultID string) error {
	return a.result.writeLearnings(params, resultID)
}
