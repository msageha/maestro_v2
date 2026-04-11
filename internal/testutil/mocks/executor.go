package mocks

import "github.com/msageha/maestro_v2/internal/daemon/core"

// MockExecutor implements core.AgentExecutor for testing.
// Calls records all Execute invocations. Result is the value returned by Execute.
type MockExecutor struct {
	Calls  []core.ExecRequest
	Result core.ExecResult
}

func (m *MockExecutor) Execute(req core.ExecRequest) core.ExecResult {
	m.Calls = append(m.Calls, req)
	return m.Result
}

func (m *MockExecutor) Close() error { return nil }
