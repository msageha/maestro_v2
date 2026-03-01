//go:build integration

package daemon

import (
	"testing"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
)

// E2EDaemon wraps *Daemon to expose internal methods for external test packages (daemon_test).
type E2EDaemon struct {
	D *Daemon
}

func NewE2ETestDaemon(t *testing.T) *E2EDaemon {
	t.Helper()
	return &E2EDaemon{D: newIntegrationDaemon(t)}
}

func (e *E2EDaemon) MaestroDir() string        { return e.D.maestroDir }
func (e *E2EDaemon) Config() model.Config       { return e.D.config }
func (e *E2EDaemon) LockMap() *lock.MutexMap    { return e.D.handler.lockMap }
func (e *E2EDaemon) PeriodicScan()              { e.D.handler.PeriodicScan() }
func (e *E2EDaemon) HandlePlan(req *uds.Request) *uds.Response {
	return e.D.handlePlan(req)
}

// Re-export integration test helpers for external test packages.

func E2EWriteCommand(t *testing.T, e *E2EDaemon, instruction string) string {
	t.Helper()
	return writeCommand(t, e.D, instruction)
}

func E2EWriteResult(t *testing.T, e *E2EDaemon, reporter, taskID, commandID, status, summary string, leaseEpoch int) string {
	t.Helper()
	return writeResult(t, e.D, reporter, taskID, commandID, status, summary, leaseEpoch)
}

func E2EReadCommandQueue(t *testing.T, e *E2EDaemon) model.CommandQueue {
	t.Helper()
	return readCommandQueue(t, e.D)
}

func E2EReadTaskQueue(t *testing.T, e *E2EDaemon, workerID string) model.TaskQueue {
	t.Helper()
	return readTaskQueue(t, e.D, workerID)
}

func E2EReadCommandState(t *testing.T, e *E2EDaemon, commandID string) model.CommandState {
	t.Helper()
	return readCommandState(t, e.D, commandID)
}

func E2EReadPlannerSignals(t *testing.T, e *E2EDaemon) model.PlannerSignalQueue {
	t.Helper()
	return readPlannerSignals(t, e.D)
}

// E2EWriteCancelRequest writes a cancel-request via queue-write and returns the raw response.
func E2EWriteCancelRequest(t *testing.T, e *E2EDaemon, commandID, reason string) *uds.Response {
	t.Helper()
	return e.D.handleQueueWrite(makeQueueWriteRequest(t, QueueWriteParams{
		Type:      "cancel-request",
		CommandID: commandID,
		Reason:    reason,
	}))
}

// E2EWriteResultRaw writes a result and returns the raw response (does not fatalf on error).
func E2EWriteResultRaw(t *testing.T, e *E2EDaemon, params ResultWriteParams) *uds.Response {
	t.Helper()
	return e.D.handleResultWrite(makeResultWriteRequest(t, params))
}

// E2ESetExecutorFactory overrides the executor factory on the queue handler.
func E2ESetExecutorFactory(e *E2EDaemon, factory func(string, model.WatcherConfig, string) (AgentExecutor, error)) {
	e.D.handler.SetExecutorFactory(factory)
}

// E2EReadResultFile reads a worker result file.
func E2EReadResultFile(t *testing.T, e *E2EDaemon, workerID string) model.TaskResultFile {
	t.Helper()
	return readResultFile(t, e.D, workerID)
}
