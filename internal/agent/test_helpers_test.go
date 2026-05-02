package agent

import (
	"bytes"
	"context"
	"log"
	"regexp"
	"strings"

	"github.com/msageha/maestro_v2/internal/model"
)

// === mockPaneIO: unified PaneIO mock for all agent package tests ===
//
// Supports three response patterns (checked in priority order):
//  1. Callback overrides (Fn fields) — for focused tests needing custom logic
//  2. Sequence responses (Seq fields) — for tests validating call order/patterns
//  3. Simple defaults (field values) — for minimal test setup
//
// All methods record calls in the `calls` slice for order assertion.
// Sequences clamp to the last element when exhausted (intentional for robustness).
type mockPaneIO struct {
	// --- Call recording ---
	calls []string

	// --- FindPaneByAgentID ---
	findPaneTarget string // default return (zero value returns "%0")
	findPaneErr    error

	// --- GetPaneCurrentCommand ---
	currentCommand string     // simple default
	getCmdSeq      []mockResp // sequence override (clamped)
	getCmdIdx      int

	// --- CapturePane ---
	captureContent string     // simple default
	capturePaneSeq []mockResp // sequence override (clamped)
	capturePaneIdx int

	// --- CapturePaneJoined ---
	joinedContent    []string // convenience rotation (wraps around)
	joinedIdx        int
	captureJoinedSeq []mockResp // full sequence with error support (clamped)
	captureJoinedIdx int

	// --- IsShellCommand ---
	isShell    bool   // simple default
	isShellSeq []bool // sequence override (clamped)
	isShellIdx int

	// --- SendCommand ---
	sentCmds      []string
	sendCmdErrSeq []error // sequence of errors (clamped)
	sendCmdIdx    int

	// --- SendTextAndSubmit ---
	sentTexts   []string
	sendTextErr error

	// --- Other error injection ---
	sendKeysErr    error
	sendCtrlCErr   error
	respawnPaneErr error

	// --- State ---
	userVars  map[string]string
	panePID   string
	getPIDErr error

	// --- Callback overrides (highest priority) ---
	FindPaneByAgentIDFn     func(agentID string) (string, error)
	SendCtrlCFn             func(paneTarget string) error
	SendKeysFn              func(paneTarget string, keys ...string) error
	SendCommandFn           func(paneTarget, command string) error
	SendTextAndSubmitFn     func(ctx context.Context, paneTarget, text string) error
	SetUserVarFn            func(paneTarget, name, value string) error
	GetUserVarFn            func(paneTarget, name string) (string, error)
	GetPanePIDFn            func(paneTarget string) (string, error)
	GetPaneCurrentCommandFn func(paneTarget string) (string, error)
	CapturePaneFn           func(paneTarget string, lastN int) (string, error)
	CapturePaneJoinedFn     func(paneTarget string, lastN int) (string, error)
	IsShellCommandFn        func(cmd string) bool
	RespawnPaneFn           func(paneTarget, startDir string) error
}

type mockResp struct {
	val string
	err error
}

// newMockPaneIO creates a mockPaneIO with sensible defaults for executor/coverage tests.
// Tests that only need busyDetector can use &mockPaneIO{} directly.
func newMockPaneIO() *mockPaneIO {
	return &mockPaneIO{
		userVars: make(map[string]string),
		panePID:  "12345",
	}
}

// seqIdx returns the clamped index for a sequence (repeats last element).
func (m *mockPaneIO) seqIdx(idx, length int) int {
	if idx >= length {
		return length - 1
	}
	return idx
}

func (m *mockPaneIO) FindPaneByAgentID(agentID string) (string, error) {
	m.calls = append(m.calls, "FindPaneByAgentID")
	if m.FindPaneByAgentIDFn != nil {
		return m.FindPaneByAgentIDFn(agentID)
	}
	if m.findPaneErr != nil {
		return "", m.findPaneErr
	}
	if m.findPaneTarget != "" {
		return m.findPaneTarget, nil
	}
	return "%0", nil
}

func (m *mockPaneIO) SendCtrlC(paneTarget string) error {
	m.calls = append(m.calls, "SendCtrlC")
	if m.SendCtrlCFn != nil {
		return m.SendCtrlCFn(paneTarget)
	}
	return m.sendCtrlCErr
}

func (m *mockPaneIO) SendKeys(paneTarget string, keys ...string) error {
	m.calls = append(m.calls, "SendKeys:"+strings.Join(keys, ","))
	if m.SendKeysFn != nil {
		return m.SendKeysFn(paneTarget, keys...)
	}
	return m.sendKeysErr
}

func (m *mockPaneIO) SendCommand(paneTarget, command string) error {
	m.calls = append(m.calls, "SendCommand:"+command)
	m.sentCmds = append(m.sentCmds, command)
	if m.SendCommandFn != nil {
		return m.SendCommandFn(paneTarget, command)
	}
	if len(m.sendCmdErrSeq) > 0 {
		idx := m.seqIdx(m.sendCmdIdx, len(m.sendCmdErrSeq))
		m.sendCmdIdx++
		return m.sendCmdErrSeq[idx]
	}
	return nil
}

func (m *mockPaneIO) SendTextAndSubmit(ctx context.Context, paneTarget, text string) error {
	m.calls = append(m.calls, "SendTextAndSubmit")
	m.sentTexts = append(m.sentTexts, text)
	if m.SendTextAndSubmitFn != nil {
		return m.SendTextAndSubmitFn(ctx, paneTarget, text)
	}
	return m.sendTextErr
}

func (m *mockPaneIO) SetUserVar(paneTarget, name, value string) error {
	if m.SetUserVarFn != nil {
		return m.SetUserVarFn(paneTarget, name, value)
	}
	if m.userVars == nil {
		m.userVars = make(map[string]string)
	}
	m.userVars[name] = value
	return nil
}

func (m *mockPaneIO) GetUserVar(paneTarget, name string) (string, error) {
	if m.GetUserVarFn != nil {
		return m.GetUserVarFn(paneTarget, name)
	}
	if m.userVars == nil {
		return "", nil
	}
	return m.userVars[name], nil
}

func (m *mockPaneIO) GetPanePID(paneTarget string) (string, error) {
	if m.GetPanePIDFn != nil {
		return m.GetPanePIDFn(paneTarget)
	}
	return m.panePID, m.getPIDErr
}

func (m *mockPaneIO) GetPaneCurrentCommand(paneTarget string) (string, error) {
	m.calls = append(m.calls, "GetPaneCurrentCommand")
	if m.GetPaneCurrentCommandFn != nil {
		return m.GetPaneCurrentCommandFn(paneTarget)
	}
	if len(m.getCmdSeq) > 0 {
		idx := m.seqIdx(m.getCmdIdx, len(m.getCmdSeq))
		m.getCmdIdx++
		return m.getCmdSeq[idx].val, m.getCmdSeq[idx].err
	}
	return m.currentCommand, nil
}

func (m *mockPaneIO) CapturePane(paneTarget string, lastN int) (string, error) {
	m.calls = append(m.calls, "CapturePane")
	if m.CapturePaneFn != nil {
		return m.CapturePaneFn(paneTarget, lastN)
	}
	if len(m.capturePaneSeq) > 0 {
		idx := m.seqIdx(m.capturePaneIdx, len(m.capturePaneSeq))
		m.capturePaneIdx++
		return m.capturePaneSeq[idx].val, m.capturePaneSeq[idx].err
	}
	return m.captureContent, nil
}

func (m *mockPaneIO) CapturePaneJoined(paneTarget string, lastN int) (string, error) {
	m.calls = append(m.calls, "CapturePaneJoined")
	if m.CapturePaneJoinedFn != nil {
		return m.CapturePaneJoinedFn(paneTarget, lastN)
	}
	if len(m.captureJoinedSeq) > 0 {
		idx := m.seqIdx(m.captureJoinedIdx, len(m.captureJoinedSeq))
		m.captureJoinedIdx++
		return m.captureJoinedSeq[idx].val, m.captureJoinedSeq[idx].err
	}
	if len(m.joinedContent) > 0 {
		content := m.joinedContent[m.joinedIdx%len(m.joinedContent)]
		m.joinedIdx++
		return content, nil
	}
	return m.captureContent, nil
}

func (m *mockPaneIO) IsShellCommand(cmd string) bool {
	if m.IsShellCommandFn != nil {
		return m.IsShellCommandFn(cmd)
	}
	if len(m.isShellSeq) > 0 {
		idx := m.seqIdx(m.isShellIdx, len(m.isShellSeq))
		m.isShellIdx++
		return m.isShellSeq[idx]
	}
	return m.isShell
}

func (m *mockPaneIO) RespawnPane(paneTarget, startDir string) error {
	m.calls = append(m.calls, "RespawnPane:"+startDir)
	if m.RespawnPaneFn != nil {
		return m.RespawnPaneFn(paneTarget, startDir)
	}
	return m.respawnPaneErr
}

// newTestBusyDetector creates a busyDetector for testing.
// Uses direct field assignment to bypass newBusyDetector's default normalization,
// allowing zero-value IdleStableSec/BusyCheckInterval for instant test execution.
func newTestBusyDetector(paneIO PaneIO, busyRegex *regexp.Regexp, cfg busyDetectorConfig) *busyDetector {
	return &busyDetector{
		paneIO:    paneIO,
		busyRegex: busyRegex,
		config:    cfg,
		logger:    log.New(&bytes.Buffer{}, "", 0),
		logLevel:  logLevelDebug,
	}
}

func fastConfig() busyDetectorConfig {
	return busyDetectorConfig{
		IdleStableSec:       0, // no sleep in tests
		BusyCheckMaxRetries: 3,
		BusyCheckInterval:   0,
	}
}

// newExecMock creates a mockPaneIO pre-configured for executor integration tests
// with common defaults (shell detected, prompt ready).
func newExecMock() *mockPaneIO {
	m := newMockPaneIO()
	m.currentCommand = "bash"
	m.isShell = true
	m.captureContent = "output\n ❯ \n"
	return m
}

// newCovExecutor creates an Executor with fast config for coverage tests.
func newCovExecutor(mock *mockPaneIO) (*Executor, *bytes.Buffer) {
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)

	cfg := model.WatcherConfig{
		BusyCheckInterval:      0,
		BusyCheckMaxRetries:    1,
		IdleStableSec:          0,
		CooldownAfterClear:     0,
		WaitReadyIntervalSec:   0,
		WaitReadyMaxRetries:    2,
		ClearConfirmTimeoutSec: 2,
		ClearConfirmPollMs:     10,
		ClearMaxAttempts:       3,
		ClearRetryBackoffMs:    1,
	}

	execCfg := DefaultExecutorConfig()
	ps := newPaneStateManager(mock)

	bd := &busyDetector{
		paneIO: mock,
		config: busyDetectorConfig{
			IdleStableSec:       0,
			BusyCheckMaxRetries: 1,
			BusyCheckInterval:   0,
			BusyHintLines:       execCfg.BusyHintLines,
		},
		logger:   logger,
		logLevel: logLevelDebug,
	}

	e := &Executor{
		execCfg:      execCfg,
		config:       cfg,
		logger:       logger,
		logLevel:     logLevelDebug,
		paneIO:       mock,
		busyDetector: bd,
		paneState:    ps,
	}
	e.processManager = newClaudeProcessManager(mock, ps, &e.config, execCfg, logger, logLevelDebug, "")
	// Tests use synthetic paths (e.g. "/project/worktree1") that never exist
	// on the test filesystem. Override dirExists to always report "exists"
	// so the stale-cwd respawn path does not fire and invert the existing
	// test expectations. Tests that exercise the stale-cwd branch override
	// this field directly.
	e.processManager.dirExists = func(string) bool { return true }
	e.deliverer = newMessageDeliverer(mock, ps, &e.config, execCfg, logger, logLevelDebug)
	return e, &buf
}

// newTestExecutorWithLog creates an Executor with a mock PaneIO and returns
// the log buffer for verification. Uses minimal-sleep config for instant tests.
func newTestExecutorWithLog(paneIO PaneIO) (*Executor, *bytes.Buffer) {
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)

	cfg := model.WatcherConfig{
		BusyCheckInterval:      1,
		BusyCheckMaxRetries:    1,
		IdleStableSec:          1,
		CooldownAfterClear:     1,
		WaitReadyIntervalSec:   1,
		WaitReadyMaxRetries:    1,
		ClearConfirmTimeoutSec: 1,
		ClearConfirmPollMs:     10,
		ClearMaxAttempts:       1,
		ClearRetryBackoffMs:    10,
	}

	execCfg := DefaultExecutorConfig()
	ps := newPaneStateManager(paneIO)

	bd := &busyDetector{
		paneIO: paneIO,
		config: busyDetectorConfig{
			IdleStableSec:       0,
			BusyCheckMaxRetries: 1,
			BusyCheckInterval:   0,
			BusyHintLines:       execCfg.BusyHintLines,
		},
		logger:   logger,
		logLevel: logLevelDebug,
	}

	exec := &Executor{
		execCfg:      execCfg,
		config:       cfg,
		logger:       logger,
		logLevel:     logLevelDebug,
		paneIO:       paneIO,
		busyDetector: bd,
		paneState:    ps,
	}
	exec.processManager = newClaudeProcessManager(paneIO, ps, &exec.config, execCfg, logger, logLevelDebug, "")
	// Match newCovExecutor: synthetic test paths never exist on disk, so
	// always report directories as present to keep stale-cwd respawn off
	// unless a test explicitly opts in.
	exec.processManager.dirExists = func(string) bool { return true }
	exec.deliverer = newMessageDeliverer(paneIO, ps, &exec.config, execCfg, logger, logLevelDebug)

	return exec, &buf
}

// callsContain checks if the call log contains a specific entry.
func callsContain(calls []string, target string) bool {
	for _, c := range calls {
		if strings.Contains(c, target) {
			return true
		}
	}
	return false
}

// isAgentLaunchCmd reports whether cmd is a maestro agent launch command,
// regardless of whether a bare name ("maestro agent launch") or absolute path
// ("/path/to/maestro agent launch") is used. ResolvedLaunchCommand() always
// appends " agent launch", so checking the suffix is reliable.
func isAgentLaunchCmd(cmd string) bool {
	return strings.HasSuffix(cmd, " agent launch")
}

// callsContainLaunchCmd reports whether the call log contains a SendCommand
// call that is a maestro agent launch command (bare or absolute-path form).
func callsContainLaunchCmd(calls []string) bool {
	const prefix = "SendCommand:"
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) && isAgentLaunchCmd(c[len(prefix):]) {
			return true
		}
	}
	return false
}
