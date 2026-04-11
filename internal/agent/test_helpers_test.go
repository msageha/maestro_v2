package agent

import (
	"bytes"
	"context"
	"log"
	"regexp"
	"strings"

	"github.com/msageha/maestro_v2/internal/model"
)

// === mockPaneIO: callback-based PaneIO mock (used by busyDetector tests) ===

// mockPaneIO implements PaneIO for testing busyDetector.
type mockPaneIO struct {
	currentCommand   string
	currentCommandFn func() (string, error)
	captureContent   string
	captureFn        func(paneTarget string, lastN int) (string, error)
	joinedContent    []string // successive CapturePaneJoined results
	joinedIdx        int
	joinedFn         func(paneTarget string, lastN int) (string, error)
	isShell          bool

	// Fn callback fields for all PaneIO methods — when non-nil, the callback
	// is invoked instead of the default behaviour (nil return).
	FindPaneByAgentIDFn func(agentID string) (string, error)
	SendCtrlCFn         func(paneTarget string) error
	SendKeysFn          func(paneTarget string, keys ...string) error
	SendCommandFn       func(paneTarget, command string) error
	SendTextAndSubmitFn func(ctx context.Context, paneTarget, text string) error
	SetUserVarFn        func(paneTarget, name, value string) error
	GetUserVarFn        func(paneTarget, name string) (string, error)
	GetPanePIDFn        func(paneTarget string) (string, error)
	IsShellCommandFn    func(cmd string) bool
	RespawnPaneFn       func(paneTarget, startDir string) error
}

func (m *mockPaneIO) FindPaneByAgentID(agentID string) (string, error) {
	if m.FindPaneByAgentIDFn != nil {
		return m.FindPaneByAgentIDFn(agentID)
	}
	return "%0", nil
}

func (m *mockPaneIO) SendCtrlC(paneTarget string) error {
	if m.SendCtrlCFn != nil {
		return m.SendCtrlCFn(paneTarget)
	}
	return nil
}

func (m *mockPaneIO) SendKeys(paneTarget string, keys ...string) error {
	if m.SendKeysFn != nil {
		return m.SendKeysFn(paneTarget, keys...)
	}
	return nil
}

func (m *mockPaneIO) SendCommand(paneTarget, command string) error {
	if m.SendCommandFn != nil {
		return m.SendCommandFn(paneTarget, command)
	}
	return nil
}

func (m *mockPaneIO) SendTextAndSubmit(ctx context.Context, paneTarget, text string) error {
	if m.SendTextAndSubmitFn != nil {
		return m.SendTextAndSubmitFn(ctx, paneTarget, text)
	}
	return nil
}

func (m *mockPaneIO) SetUserVar(paneTarget, name, value string) error {
	if m.SetUserVarFn != nil {
		return m.SetUserVarFn(paneTarget, name, value)
	}
	return nil
}

func (m *mockPaneIO) GetUserVar(paneTarget, name string) (string, error) {
	if m.GetUserVarFn != nil {
		return m.GetUserVarFn(paneTarget, name)
	}
	return "", nil
}

func (m *mockPaneIO) GetPanePID(paneTarget string) (string, error) {
	if m.GetPanePIDFn != nil {
		return m.GetPanePIDFn(paneTarget)
	}
	return "12345", nil
}

func (m *mockPaneIO) GetPaneCurrentCommand(paneTarget string) (string, error) {
	if m.currentCommandFn != nil {
		return m.currentCommandFn()
	}
	return m.currentCommand, nil
}

func (m *mockPaneIO) CapturePane(paneTarget string, lastN int) (string, error) {
	if m.captureFn != nil {
		return m.captureFn(paneTarget, lastN)
	}
	return m.captureContent, nil
}

func (m *mockPaneIO) CapturePaneJoined(paneTarget string, lastN int) (string, error) {
	if m.joinedFn != nil {
		return m.joinedFn(paneTarget, lastN)
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
	return m.isShell
}

func (m *mockPaneIO) RespawnPane(paneTarget, startDir string) error {
	if m.RespawnPaneFn != nil {
		return m.RespawnPaneFn(paneTarget, startDir)
	}
	return nil
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

// === covMockPaneIO: sequence-based PaneIO mock (used by executor coverage tests) ===

// covMockPaneIO supports sequence-based responses for thorough coverage tests.
type covMockPaneIO struct {
	capturePaneSeq   []mockResp
	capturePaneIdx   int
	captureJoinedSeq []mockResp
	captureJoinedIdx int
	sendCmdErrSeq    []error
	sendCmdIdx       int
	sentCmds         []string
	sendKeysErr      error
	sendCtrlCErr     error
	respawnPaneErr   error
	sendTextErr      error
	sentTexts        []string
	getCmdSeq        []mockResp
	getCmdIdx        int
	isShellSeq       []bool
	isShellIdx       int
	defaultIsShell   bool
	userVars         map[string]string
	panePID          string
	getPIDErr        error
	calls            []string
}

type mockResp struct {
	val string
	err error
}

func newCovMock() *covMockPaneIO {
	return &covMockPaneIO{
		userVars:       make(map[string]string),
		panePID:        "12345",
		defaultIsShell: true,
	}
}

func (m *covMockPaneIO) seqIdx(idx, length int) int {
	if idx >= length {
		return length - 1
	}
	return idx
}

func (m *covMockPaneIO) FindPaneByAgentID(_ string) (string, error) { return "%0", nil }

func (m *covMockPaneIO) SendCtrlC(_ string) error {
	m.calls = append(m.calls, "SendCtrlC")
	return m.sendCtrlCErr
}

func (m *covMockPaneIO) SendKeys(_ string, keys ...string) error {
	m.calls = append(m.calls, "SendKeys:"+strings.Join(keys, ","))
	return m.sendKeysErr
}

func (m *covMockPaneIO) SendCommand(_, cmd string) error {
	m.calls = append(m.calls, "SendCommand:"+cmd)
	m.sentCmds = append(m.sentCmds, cmd)
	if len(m.sendCmdErrSeq) > 0 {
		idx := m.seqIdx(m.sendCmdIdx, len(m.sendCmdErrSeq))
		m.sendCmdIdx++
		return m.sendCmdErrSeq[idx]
	}
	return nil
}

func (m *covMockPaneIO) SendTextAndSubmit(_ context.Context, _, text string) error {
	m.calls = append(m.calls, "SendTextAndSubmit")
	m.sentTexts = append(m.sentTexts, text)
	return m.sendTextErr
}

func (m *covMockPaneIO) SetUserVar(_, name, value string) error {
	m.userVars[name] = value
	return nil
}

func (m *covMockPaneIO) GetUserVar(_, name string) (string, error) {
	return m.userVars[name], nil
}

func (m *covMockPaneIO) GetPanePID(_ string) (string, error) {
	return m.panePID, m.getPIDErr
}

func (m *covMockPaneIO) GetPaneCurrentCommand(_ string) (string, error) {
	m.calls = append(m.calls, "GetPaneCurrentCommand")
	if len(m.getCmdSeq) > 0 {
		idx := m.seqIdx(m.getCmdIdx, len(m.getCmdSeq))
		m.getCmdIdx++
		return m.getCmdSeq[idx].val, m.getCmdSeq[idx].err
	}
	return "bash", nil
}

func (m *covMockPaneIO) CapturePane(_ string, _ int) (string, error) {
	m.calls = append(m.calls, "CapturePane")
	if len(m.capturePaneSeq) > 0 {
		idx := m.seqIdx(m.capturePaneIdx, len(m.capturePaneSeq))
		m.capturePaneIdx++
		return m.capturePaneSeq[idx].val, m.capturePaneSeq[idx].err
	}
	return "output\n \u276f \n", nil
}

func (m *covMockPaneIO) CapturePaneJoined(_ string, _ int) (string, error) {
	m.calls = append(m.calls, "CapturePaneJoined")
	if len(m.captureJoinedSeq) > 0 {
		idx := m.seqIdx(m.captureJoinedIdx, len(m.captureJoinedSeq))
		m.captureJoinedIdx++
		return m.captureJoinedSeq[idx].val, m.captureJoinedSeq[idx].err
	}
	return "output\n \u276f \n", nil
}

func (m *covMockPaneIO) IsShellCommand(_ string) bool {
	if len(m.isShellSeq) > 0 {
		idx := m.seqIdx(m.isShellIdx, len(m.isShellSeq))
		m.isShellIdx++
		return m.isShellSeq[idx]
	}
	return m.defaultIsShell
}

func (m *covMockPaneIO) RespawnPane(_, startDir string) error {
	m.calls = append(m.calls, "RespawnPane:"+startDir)
	return m.respawnPaneErr
}

// newCovExecutor creates an Executor with fast config for coverage tests.
func newCovExecutor(mock *covMockPaneIO) (*Executor, *bytes.Buffer) {
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
	e.processManager = newClaudeProcessManager(mock, ps, &e.config, execCfg, logger, logLevelDebug)
	e.deliverer = newMessageDeliverer(mock, ps, &e.config, execCfg, logger, logLevelDebug)
	return e, &buf
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
