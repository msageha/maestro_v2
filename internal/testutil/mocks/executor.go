// Package mocks provides minimal stand-in implementations of core interfaces
// (AgentExecutor, etc.) used by tests that need a controllable boundary
// without spinning up tmux / claude / git.
package mocks

import "github.com/msageha/maestro_v2/internal/model"

// MockExecutor implements core.AgentExecutor for testing.
// Calls records all Execute invocations. Result is the value returned by
// Execute. ResultByCall, when set, returns the i-th element for the i-th
// call (with later calls falling back to Result); use it to drive multi-
// attempt retry tests deterministically.
// RespawnedWorkers records every workerID passed to
// RespawnPaneToProjectRoot in call order — tests that exercise Phase B
// cleanup can assert the daemon invoked the respawn hook for every
// worker associated with the worktree before tearing it down.
type MockExecutor struct {
	Calls             []model.ExecRequest
	Result            model.ExecResult
	ResultByCall      []model.ExecResult
	RespawnedWorkers  []string
	RespawnReturnsErr error
}

// Execute records the request and returns the pre-configured Result.
// When ResultByCall is set and the call index is in range, that per-call
// result is returned instead.
func (m *MockExecutor) Execute(req model.ExecRequest) model.ExecResult {
	idx := len(m.Calls)
	m.Calls = append(m.Calls, req)
	if idx < len(m.ResultByCall) {
		return m.ResultByCall[idx]
	}
	return m.Result
}

// RespawnPaneToProjectRoot records the workerID and returns the
// configured error (default nil). Tests that need to simulate a tmux
// failure set RespawnReturnsErr.
func (m *MockExecutor) RespawnPaneToProjectRoot(workerID string) error {
	m.RespawnedWorkers = append(m.RespawnedWorkers, workerID)
	return m.RespawnReturnsErr
}

// Close is a no-op stub satisfying core.AgentExecutor.
func (m *MockExecutor) Close() error { return nil }
