// Package agent provides agent lifecycle management and command execution.
package agent

import (
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/tmux"
)

// ExecMode represents the agent execution mode.
type ExecMode string

const (
	ModeDeliver   ExecMode = "deliver"    // Direct delivery with busy check (Planner/Orchestrator)
	ModeWithClear ExecMode = "with_clear" // /clear + delivery (Workers)
	ModeInterrupt ExecMode = "interrupt"  // Interrupt running task
	ModeIsBusy    ExecMode = "is_busy"    // Query busy state only
	ModeClear     ExecMode = "clear"      // Context reset without delivery
)

// BusyVerdict is the result of busy detection.
type BusyVerdict int

const (
	VerdictIdle BusyVerdict = iota
	VerdictBusy
	VerdictUndecided
)

func (v BusyVerdict) String() string {
	switch v {
	case VerdictIdle:
		return "idle"
	case VerdictBusy:
		return "busy"
	default:
		return "undecided"
	}
}

// ExecRequest contains parameters for executing a message delivery.
type ExecRequest struct {
	AgentID    string
	Message    string
	Mode       ExecMode
	TaskID     string
	CommandID  string
	LeaseEpoch int
	Attempt    int
}

// ExecResult contains the outcome of an execution attempt.
type ExecResult struct {
	Success   bool
	Retryable bool
	Error     error
}

// LogLevel controls logging verbosity.
type LogLevel int

const (
	LogLevelDebug LogLevel = iota
	LogLevelInfo
	LogLevelWarn
	LogLevelError
)

func parseLogLevel(s string) LogLevel {
	switch strings.ToLower(s) {
	case "debug":
		return LogLevelDebug
	case "info":
		return LogLevelInfo
	case "warn", "warning":
		return LogLevelWarn
	case "error":
		return LogLevelError
	default:
		return LogLevelInfo
	}
}

// Executor handles message delivery to agents via tmux panes.
type Executor struct {
	maestroDir string
	config     model.WatcherConfig
	logger     *log.Logger
	logFile    io.Closer
	logLevel   LogLevel
	busyRegex  *regexp.Regexp
}

// NewExecutor creates a new Executor that logs to .maestro/logs/agent_executor.log.
func NewExecutor(maestroDir string, watcherCfg model.WatcherConfig, logLevel string) (*Executor, error) {
	logPath := filepath.Join(maestroDir, "logs", "agent_executor.log")
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("open log file %s: %w", logPath, err)
	}
	return newExecutor(maestroDir, watcherCfg, logLevel, logFile, logFile)
}

// newExecutor is the internal constructor that accepts an io.Writer for testing.
func newExecutor(maestroDir string, watcherCfg model.WatcherConfig, logLevel string, w io.Writer, closer io.Closer) (*Executor, error) {
	var busyRegex *regexp.Regexp
	if watcherCfg.BusyPatterns != "" {
		re, err := regexp.Compile(watcherCfg.BusyPatterns)
		if err != nil {
			return nil, fmt.Errorf("compile busy_patterns %q: %w", watcherCfg.BusyPatterns, err)
		}
		busyRegex = re
	}

	return &Executor{
		maestroDir: maestroDir,
		config:     applyDefaults(watcherCfg),
		logger:     log.New(w, "", 0),
		logFile:    closer,
		logLevel:   parseLogLevel(logLevel),
		busyRegex:  busyRegex,
	}, nil
}

// Close releases the log file handle.
func (e *Executor) Close() error {
	if e.logFile != nil {
		return e.logFile.Close()
	}
	return nil
}

func applyDefaults(cfg model.WatcherConfig) model.WatcherConfig {
	if cfg.BusyCheckInterval <= 0 {
		cfg.BusyCheckInterval = 2
	}
	if cfg.BusyCheckMaxRetries <= 0 {
		cfg.BusyCheckMaxRetries = 30
	}
	if cfg.IdleStableSec <= 0 {
		cfg.IdleStableSec = 5
	}
	if cfg.CooldownAfterClear <= 0 {
		cfg.CooldownAfterClear = 3
	}
	return cfg
}

// Execute dispatches the request based on its Mode.
func (e *Executor) Execute(req ExecRequest) ExecResult {
	paneTarget, err := tmux.FindPaneByAgentID(req.AgentID)
	if err != nil {
		e.log(LogLevelError, "delivery_error agent_id=%s error=pane_not_found: %v", req.AgentID, err)
		return ExecResult{Error: fmt.Errorf("find pane for %s: %w", req.AgentID, err)}
	}

	switch req.Mode {
	case ModeIsBusy:
		return e.execIsBusy(paneTarget)
	case ModeClear:
		return e.execClear(req, paneTarget)
	case ModeInterrupt:
		return e.execInterrupt(req, paneTarget)
	case ModeWithClear:
		return e.execWithClear(req, paneTarget)
	case ModeDeliver:
		return e.execDeliver(req, paneTarget)
	default:
		return ExecResult{Error: fmt.Errorf("unknown exec mode: %s", req.Mode)}
	}
}

// execIsBusy checks agent busy state. Returns Success=true if busy, false if idle.
func (e *Executor) execIsBusy(paneTarget string) ExecResult {
	verdict := e.detectBusy(paneTarget)
	return ExecResult{Success: verdict != VerdictIdle}
}

// execClear sends /clear and waits for stability.
func (e *Executor) execClear(req ExecRequest, paneTarget string) ExecResult {
	e.log(LogLevelDebug, "clear_operation agent_id=%s mode=clear", req.AgentID)

	if err := tmux.SendCommand(paneTarget, "/clear"); err != nil {
		return ExecResult{Error: fmt.Errorf("send /clear: %w", err)}
	}
	time.Sleep(time.Duration(e.config.CooldownAfterClear) * time.Second)

	if err := e.waitStable(paneTarget); err != nil {
		return ExecResult{Error: err, Retryable: true}
	}
	return ExecResult{Success: true}
}

// execInterrupt interrupts a running task: C-c → cooldown → /clear → cooldown → stability.
func (e *Executor) execInterrupt(req ExecRequest, paneTarget string) ExecResult {
	e.log(LogLevelInfo, "interrupt_start agent_id=%s task_id=%s lease_epoch=%d",
		req.AgentID, req.TaskID, req.LeaseEpoch)

	if req.AgentID == "orchestrator" {
		e.log(LogLevelError, "delivery_error agent_id=orchestrator error=cannot_interrupt_orchestrator")
		return ExecResult{Error: fmt.Errorf("cannot interrupt orchestrator")}
	}

	// Step 1: C-c
	if err := tmux.SendCtrlC(paneTarget); err != nil {
		return ExecResult{Error: fmt.Errorf("send C-c: %w", err)}
	}
	time.Sleep(time.Duration(e.config.CooldownAfterClear) * time.Second)

	// Step 2: /clear
	if err := tmux.SendCommand(paneTarget, "/clear"); err != nil {
		return ExecResult{Error: fmt.Errorf("send /clear: %w", err)}
	}
	time.Sleep(time.Duration(e.config.CooldownAfterClear) * time.Second)

	// Step 3: Confirm stability
	if err := e.waitStable(paneTarget); err != nil {
		return ExecResult{Error: err, Retryable: true}
	}

	// Step 4: Update @status
	if err := tmux.SetUserVar(paneTarget, "status", "busy"); err != nil {
		e.log(LogLevelWarn, "set_status_failed agent_id=%s error=%v", req.AgentID, err)
	}

	e.log(LogLevelInfo, "interrupt_success agent_id=%s task_id=%s lease_epoch=%d",
		req.AgentID, req.TaskID, req.LeaseEpoch)
	return ExecResult{Success: true}
}

// execWithClear delivers a message with prior /clear (Worker mode).
func (e *Executor) execWithClear(req ExecRequest, paneTarget string) ExecResult {
	e.log(LogLevelInfo, "delivery_start agent_id=%s task_id=%s command_id=%s lease_epoch=%d attempt=%d",
		req.AgentID, req.TaskID, req.CommandID, req.LeaseEpoch, req.Attempt)

	// Orchestrator: never /clear, fall through to deliver mode
	if req.AgentID == "orchestrator" {
		return e.execDeliver(req, paneTarget)
	}

	// Step 1: /clear
	e.log(LogLevelDebug, "clear_operation agent_id=%s mode=with_clear", req.AgentID)
	if err := tmux.SendCommand(paneTarget, "/clear"); err != nil {
		return ExecResult{Error: fmt.Errorf("send /clear: %w", err)}
	}
	time.Sleep(time.Duration(e.config.CooldownAfterClear) * time.Second)

	// Step 2: Stability check
	if err := e.waitStable(paneTarget); err != nil {
		e.log(LogLevelWarn, "delivery_failure agent_id=%s task_id=%s reason=unstable_after_clear",
			req.AgentID, req.TaskID)
		return ExecResult{Error: err, Retryable: true}
	}

	// Step 3: Busy detection with retry
	verdict := e.detectBusyWithRetry(req, paneTarget)
	if verdict != VerdictIdle {
		reason := "busy_timeout"
		if verdict == VerdictUndecided {
			reason = "undecided_after_probes"
		}
		e.log(LogLevelWarn, "delivery_failure agent_id=%s task_id=%s reason=%s",
			req.AgentID, req.TaskID, reason)
		return ExecResult{
			Error:     fmt.Errorf("agent %s busy: %s", req.AgentID, reason),
			Retryable: true,
		}
	}

	// Step 4: Deliver
	return e.sendAndConfirm(req, paneTarget)
}

// execDeliver delivers a message without /clear (Planner/Orchestrator).
func (e *Executor) execDeliver(req ExecRequest, paneTarget string) ExecResult {
	e.log(LogLevelInfo, "delivery_start agent_id=%s task_id=%s command_id=%s lease_epoch=%d attempt=%d",
		req.AgentID, req.TaskID, req.CommandID, req.LeaseEpoch, req.Attempt)

	// Orchestrator: strict busy check, no retry, immediate failure if busy
	if req.AgentID == "orchestrator" {
		verdict := e.detectBusy(paneTarget)
		e.log(LogLevelDebug, "busy_detection agent_id=orchestrator verdict=%s", verdict)
		if verdict != VerdictIdle {
			e.log(LogLevelWarn, "delivery_failure agent_id=orchestrator reason=orchestrator_busy verdict=%s", verdict)
			return ExecResult{
				Error:     fmt.Errorf("orchestrator busy (verdict=%s)", verdict),
				Retryable: true,
			}
		}
		return e.sendAndConfirm(req, paneTarget)
	}

	// Planner/other: busy detection with retry
	verdict := e.detectBusyWithRetry(req, paneTarget)
	if verdict != VerdictIdle {
		reason := "busy_timeout"
		if verdict == VerdictUndecided {
			reason = "undecided_after_probes"
		}
		e.log(LogLevelWarn, "delivery_failure agent_id=%s task_id=%s reason=%s",
			req.AgentID, req.TaskID, reason)
		return ExecResult{
			Error:     fmt.Errorf("agent %s busy: %s", req.AgentID, reason),
			Retryable: true,
		}
	}

	return e.sendAndConfirm(req, paneTarget)
}

// sendAndConfirm sends the message and updates @status to busy.
// For non-orchestrator agents, it first sends C-c to clear partial input.
func (e *Executor) sendAndConfirm(req ExecRequest, paneTarget string) ExecResult {
	// Orchestrator pane protection: never send C-c to orchestrator
	if req.AgentID != "orchestrator" {
		if err := tmux.SendCtrlC(paneTarget); err != nil {
			e.log(LogLevelWarn, "cleanup_ctrlc_failed agent_id=%s error=%v", req.AgentID, err)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Send message via paste-buffer + Enter for reliable multi-line delivery
	if err := tmux.SendTextAndSubmit(paneTarget, req.Message); err != nil {
		e.log(LogLevelError, "delivery_error agent_id=%s task_id=%s error=send_text: %v",
			req.AgentID, req.TaskID, err)
		return ExecResult{Error: fmt.Errorf("send message: %w", err)}
	}

	// Update @status to busy
	if err := tmux.SetUserVar(paneTarget, "status", "busy"); err != nil {
		e.log(LogLevelWarn, "set_status_failed agent_id=%s error=%v", req.AgentID, err)
	}

	e.log(LogLevelInfo, "delivery_success agent_id=%s task_id=%s command_id=%s lease_epoch=%d",
		req.AgentID, req.TaskID, req.CommandID, req.LeaseEpoch)
	return ExecResult{Success: true}
}

// shellCommands lists commands that indicate no agent CLI is running.
var shellCommands = map[string]bool{
	"bash": true, "zsh": true, "fish": true,
	"sh": true, "dash": true, "tcsh": true, "csh": true,
}

// --- Busy Detection ---

// detectBusy performs one round of the 3-stage busy detection algorithm.
func (e *Executor) detectBusy(paneTarget string) BusyVerdict {
	// Stage 1: pane_current_command — quick gate
	cmd, err := tmux.GetPaneCurrentCommand(paneTarget)
	if err != nil {
		e.log(LogLevelDebug, "busy_detection pane_current_command error=%v", err)
		return VerdictUndecided
	}
	e.log(LogLevelDebug, "busy_detection started; pane_current_command=%s", cmd)

	// If the pane is running only a shell, no agent CLI is active → idle.
	if shellCommands[cmd] {
		e.log(LogLevelDebug, "busy_detection pane running shell %q → idle", cmd)
		return VerdictIdle
	}

	// Stage 2: Pattern hint from last 3 lines
	content, err := tmux.CapturePane(paneTarget, 3)
	if err != nil {
		e.log(LogLevelDebug, "busy_detection capture_pane error=%v", err)
		return VerdictUndecided
	}

	patternMatched := e.busyRegex != nil && e.busyRegex.MatchString(content)

	hintStr := "not_matched"
	if patternMatched {
		hintStr = "matched"
	}
	e.log(LogLevelDebug, "busy_detection busy_pattern_hint=%s", hintStr)

	// Stage 3: Activity probe (hash comparison over idle_stable_sec)
	hashA := contentHash(content)
	time.Sleep(time.Duration(e.config.IdleStableSec) * time.Second)

	content2, err := tmux.CapturePane(paneTarget, 3)
	if err != nil {
		e.log(LogLevelDebug, "busy_detection second capture error=%v", err)
		return VerdictUndecided
	}
	hashB := contentHash(content2)

	hashChanged := hashA != hashB

	var verdict BusyVerdict
	switch {
	case hashChanged:
		verdict = VerdictBusy
	case !patternMatched:
		verdict = VerdictIdle
	default:
		verdict = VerdictUndecided
	}

	e.log(LogLevelDebug, "busy_detection activity_probe hash_changed=%v verdict=%s",
		hashChanged, verdict)
	return verdict
}

// detectBusyWithRetry runs busy detection with a retry loop on VerdictBusy.
func (e *Executor) detectBusyWithRetry(req ExecRequest, paneTarget string) BusyVerdict {
	verdict := e.detectBusy(paneTarget)
	if verdict != VerdictBusy {
		return verdict
	}

	for i := 1; i <= e.config.BusyCheckMaxRetries; i++ {
		e.log(LogLevelDebug, "busy_retry retry=%d/%d agent_id=%s",
			i, e.config.BusyCheckMaxRetries, req.AgentID)
		time.Sleep(time.Duration(e.config.BusyCheckInterval) * time.Second)

		verdict = e.detectBusy(paneTarget)
		if verdict != VerdictBusy {
			return verdict
		}
	}

	return VerdictBusy
}

// waitStable confirms pane content doesn't change over idle_stable_sec.
func (e *Executor) waitStable(paneTarget string) error {
	content, err := tmux.CapturePane(paneTarget, 3)
	if err != nil {
		return fmt.Errorf("capture pane for stability: %w", err)
	}
	h1 := contentHash(content)

	time.Sleep(time.Duration(e.config.IdleStableSec) * time.Second)

	content2, err := tmux.CapturePane(paneTarget, 3)
	if err != nil {
		return fmt.Errorf("capture pane for stability: %w", err)
	}
	h2 := contentHash(content2)

	if h1 != h2 {
		return fmt.Errorf("pane content not stable after %ds", e.config.IdleStableSec)
	}
	return nil
}

// contentHash returns a hex-encoded SHA-256 hash of the content.
func contentHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h)
}

// --- Envelope Builders ---

// BuildWorkerEnvelope creates the delivery envelope for a Worker task.
// Format matches spec §5.8.1 Worker 向けタスク配信エンベロープ.
func BuildWorkerEnvelope(task model.Task, workerID string, leaseEpoch, attempt int) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "[maestro] task_id:%s command_id:%s lease_epoch:%d attempt:%d\n",
		task.ID, task.CommandID, leaseEpoch, attempt)
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "purpose: %s\n", task.Purpose)
	fmt.Fprintf(&sb, "content: %s\n", task.Content)
	fmt.Fprintf(&sb, "acceptance_criteria: %s\n", task.AcceptanceCriteria)
	constraintsStr := "なし"
	if len(task.Constraints) > 0 {
		constraintsStr = strings.Join(task.Constraints, ", ")
	}
	fmt.Fprintf(&sb, "constraints: %s\n", constraintsStr)
	toolsHintStr := "なし"
	if len(task.ToolsHint) > 0 {
		toolsHintStr = strings.Join(task.ToolsHint, ", ")
	}
	fmt.Fprintf(&sb, "tools_hint: %s\n", toolsHintStr)
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "完了時: maestro result write %s --task-id %s --command-id %s --lease-epoch %d --status <completed|failed> --summary \"...\"\n",
		workerID, task.ID, task.CommandID, leaseEpoch)
	sb.WriteString("失敗時に部分変更あり: 上記に加えて --partial-changes --no-retry-safe")
	return sb.String()
}

// BuildPlannerEnvelope creates the delivery envelope for a Planner command.
// Format matches spec §5.8.1 Planner 向けコマンド配信エンベロープ.
func BuildPlannerEnvelope(cmd model.Command, leaseEpoch, attempt int) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "[maestro] command_id:%s lease_epoch:%d attempt:%d\n",
		cmd.ID, leaseEpoch, attempt)
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "content: %s\n", cmd.Content)
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "タスク分解後: maestro plan submit --command-id %s --tasks-file plan.yaml\n", cmd.ID)
	fmt.Fprintf(&sb, "全タスク完了後: maestro plan complete --command-id %s --summary \"...\"", cmd.ID)
	return sb.String()
}

// BuildOrchestratorNotificationEnvelope creates the envelope for an Orchestrator notification.
// Format matches spec §5.8.1 Orchestrator 向け通知配信エンベロープ.
func BuildOrchestratorNotificationEnvelope(commandID, notificationType string) string {
	terminalStatus := mapNotificationTypeToStatus(notificationType)
	return fmt.Sprintf("[maestro] kind:command_completed command_id:%s status:%s\nresults/planner.yaml を確認してください",
		commandID, terminalStatus)
}

func mapNotificationTypeToStatus(nt string) string {
	switch nt {
	case "command_completed":
		return "completed"
	case "command_failed":
		return "failed"
	case "command_cancelled":
		return "cancelled"
	default:
		return nt
	}
}

// BuildTaskResultNotification creates a side-channel notification for the Planner.
func BuildTaskResultNotification(commandID, taskID, workerID, taskStatus string) string {
	return fmt.Sprintf("[maestro] kind:task_result command_id:%s task_id:%s worker_id:%s status:%s\nresults/%s.yaml を確認してください",
		commandID, taskID, workerID, taskStatus, workerID)
}

// --- Logging ---

func (e *Executor) log(level LogLevel, format string, args ...any) {
	if level < e.logLevel {
		return
	}
	levelStr := "INFO"
	switch level {
	case LogLevelDebug:
		levelStr = "DEBUG"
	case LogLevelWarn:
		levelStr = "WARN"
	case LogLevelError:
		levelStr = "ERROR"
	}
	msg := fmt.Sprintf(format, args...)
	e.logger.Printf("%s %s agent_executor: %s", time.Now().Format(time.RFC3339), levelStr, msg)
}
