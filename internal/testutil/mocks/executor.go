// Package mocks provides minimal stand-in implementations of core interfaces
// (AgentExecutor, etc.) used by tests that need a controllable boundary
// without spinning up tmux / claude / git.
package mocks

import "github.com/msageha/maestro_v2/internal/model"

// MockExecutor implements core.AgentExecutor for testing.
// Calls records all Execute invocations. Result is the value returned by Execute.
type MockExecutor struct {
	Calls  []model.ExecRequest
	Result model.ExecResult
}

// Execute records the request and returns the pre-configured Result.
func (m *MockExecutor) Execute(req model.ExecRequest) model.ExecResult {
	m.Calls = append(m.Calls, req)
	return m.Result
}

// Close is a no-op stub satisfying core.AgentExecutor.
func (m *MockExecutor) Close() error { return nil }
