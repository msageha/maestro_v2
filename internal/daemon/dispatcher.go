package daemon

import (
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/daemon/learnings"
	"github.com/msageha/maestro_v2/internal/daemon/persona"
	"github.com/msageha/maestro_v2/internal/daemon/skill"
	"github.com/msageha/maestro_v2/internal/events"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/templates"
)

// errExecutorInit is re-exported from core via core_aliases.go.

// maxGateEvaluations is the maximum number of gate evaluation entries kept in memory.
const maxGateEvaluations = 1000

// Dispatcher handles priority sorting and agent_executor dispatch.
type Dispatcher struct {
	maestroDir        string
	config            model.Config
	leaseManager      *LeaseManager
	dl                *DaemonLogger
	logger            *log.Logger
	logLevel          LogLevel
	clock             Clock
	executorFactory   ExecutorFactory
	mu                sync.RWMutex // protects eventBus, qualityGate, worktreeManager
	eventBus          *events.Bus
	qualityGate       *QualityGateDaemon
	gateEvaluations   map[string]*model.QualityGateEvaluation // task_id -> evaluation
	gateEvalMutex     sync.RWMutex

	// cachedExec is a shared Executor instance created once and reused across
	// dispatch calls. This avoids per-dispatch log file Open/Close overhead.
	// All access is protected by execMu to prevent data races during shutdown
	// (CloseExecutor) and testing (SetExecutorFactory).
	execMu           sync.Mutex
	cachedExec       AgentExecutor
	execInit         bool
	execFailCount    int
	execLastFailTime time.Time

	worktreeManager *WorktreeManager
}

// ExecutorFactory, AgentExecutor are defined in internal/daemon/core
// and re-exported via core_aliases.go.

// NewDispatcher creates a new Dispatcher.
func NewDispatcher(maestroDir string, cfg model.Config, lm *LeaseManager, logger *log.Logger, logLevel LogLevel) *Dispatcher {
	return &Dispatcher{
		maestroDir:      maestroDir,
		config:          cfg,
		leaseManager:    lm,
		dl:              NewDaemonLoggerFromLegacy("dispatcher", logger, logLevel),
		logger:          logger,
		logLevel:        logLevel,
		clock:           RealClock{},
		gateEvaluations: make(map[string]*model.QualityGateEvaluation),
		executorFactory: func(dir string, wcfg model.WatcherConfig, level string) (AgentExecutor, error) {
			return agent.NewExecutor(dir, wcfg, level)
		},
	}
}

// SetExecutorFactory overrides the executor factory for testing.
// Resets the cached executor so the new factory is used on next call.
func (d *Dispatcher) SetExecutorFactory(f ExecutorFactory) {
	d.execMu.Lock()
	old := d.cachedExec
	d.executorFactory = f
	d.cachedExec = nil
	d.execInit = false
	d.execFailCount = 0
	d.execLastFailTime = time.Time{}
	// Close old executor while still holding the lock to prevent getExecutor()
	// from racing between state reset and Close() completion.
	if old != nil {
		_ = old.Close()
	}
	d.execMu.Unlock()
}

// getExecutor returns the shared executor instance, creating it lazily on first call.
// On failure, the error is NOT cached — subsequent calls will retry with exponential
// backoff to avoid hammering a persistently broken resource.
// The Executor is safe for concurrent use (log.Logger uses internal mutex,
// os.File in append mode is POSIX-safe, all other fields are immutable).
func (d *Dispatcher) getExecutor() (AgentExecutor, error) {
	d.execMu.Lock()
	defer d.execMu.Unlock()
	if d.execInit {
		return d.cachedExec, nil
	}
	// Exponential backoff cooldown for consecutive failures (cap at 16s)
	if d.execFailCount > 0 {
		backoff := time.Duration(1<<min(d.execFailCount-1, 4)) * time.Second // 1s, 2s, 4s, 8s, 16s (cap)
		elapsed := d.clock.Now().Sub(d.execLastFailTime)
		if elapsed < backoff {
			return nil, fmt.Errorf("%w: retry cooldown (attempt %d, next retry in %v)", errExecutorInit, d.execFailCount, backoff-elapsed)
		}
	}
	exec, err := d.executorFactory(d.maestroDir, d.config.Watcher, d.config.Logging.Level)
	if err != nil {
		d.execFailCount++
		d.execLastFailTime = d.clock.Now()
		return nil, fmt.Errorf("%w: %v", errExecutorInit, err)
	}
	// Success: cache the executor and reset failure state
	d.cachedExec = exec
	d.execInit = true
	d.execFailCount = 0
	return d.cachedExec, nil
}

// CloseExecutor releases the shared executor's resources (log file handle).
// Safe to call multiple times; subsequent calls are no-ops.
func (d *Dispatcher) CloseExecutor() {
	d.execMu.Lock()
	exec := d.cachedExec
	d.cachedExec = nil
	d.execInit = false
	d.execFailCount = 0
	d.execLastFailTime = time.Time{}
	d.execMu.Unlock()

	if exec != nil {
		_ = exec.Close()
	}
}

// SetEventBus sets the event bus for publishing events.
func (d *Dispatcher) SetEventBus(bus *events.Bus) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.eventBus = bus
}

// SetQualityGate sets the quality gate daemon for the dispatcher.
func (d *Dispatcher) SetQualityGate(qg *QualityGateDaemon) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.qualityGate = qg
}

// SetWorktreeManager wires the worktree manager for worker path resolution during dispatch.
func (d *Dispatcher) SetWorktreeManager(wm *WorktreeManager) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.worktreeManager = wm
}

// getEventBus returns the event bus with proper synchronization.
func (d *Dispatcher) getEventBus() *events.Bus {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.eventBus
}

// getQualityGate returns the quality gate daemon with proper synchronization.
func (d *Dispatcher) getQualityGate() *QualityGateDaemon {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.qualityGate
}

// getWorktreeManager returns the worktree manager with proper synchronization.
func (d *Dispatcher) getWorktreeManager() *WorktreeManager {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.worktreeManager
}

// sortableEntry wraps a queue entry with computed effective priority for sorting.
type sortableEntry struct {
	Index             int
	EffectivePriority int
	CreatedAt         time.Time
	ID                string
}

// sortKey holds the fields extracted from a queue entry for pending-sort filtering.
type sortKey struct {
	Status    model.Status
	Priority  int
	CreatedAt string
	ID        string
}

// EffectivePriority computes the aging-adjusted priority.
// effective_priority = max(0, priority - floor(age_seconds / priority_aging_sec))
func EffectivePriority(priority int, createdAt string, priorityAgingSec int) int {
	if priorityAgingSec <= 0 {
		return priority
	}
	created, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return priority
	}
	ageSec := time.Since(created).Seconds()
	aging := int(math.Floor(ageSec / float64(priorityAgingSec)))
	result := priority - aging
	if result < 0 {
		return 0
	}
	return result
}

// sortPendingIndices filters pending items and returns their original indices
// sorted by effective_priority ASC → created_at ASC → id ASC.
func sortPendingIndices[T any](items []T, extract func(T) sortKey, priorityAgingSec int) []int {
	var entries []sortableEntry
	for i, item := range items {
		key := extract(item)
		if key.Status != model.StatusPending {
			continue
		}
		created, _ := time.Parse(time.RFC3339, key.CreatedAt)
		entries = append(entries, sortableEntry{
			Index:             i,
			EffectivePriority: EffectivePriority(key.Priority, key.CreatedAt, priorityAgingSec),
			CreatedAt:         created,
			ID:                key.ID,
		})
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].EffectivePriority != entries[j].EffectivePriority {
			return entries[i].EffectivePriority < entries[j].EffectivePriority
		}
		if !entries[i].CreatedAt.Equal(entries[j].CreatedAt) {
			return entries[i].CreatedAt.Before(entries[j].CreatedAt)
		}
		return entries[i].ID < entries[j].ID
	})

	indices := make([]int, len(entries))
	for i, e := range entries {
		indices[i] = e.Index
	}
	return indices
}

// SortPendingTasks sorts tasks by effective_priority ASC → created_at ASC → id ASC.
func (d *Dispatcher) SortPendingTasks(tasks []model.Task) []int {
	return sortPendingIndices(tasks, func(t model.Task) sortKey {
		return sortKey{Status: t.Status, Priority: t.Priority, CreatedAt: t.CreatedAt, ID: t.ID}
	}, d.config.Queue.PriorityAgingSec)
}

// SortPendingCommands sorts commands by effective_priority ASC → created_at ASC → id ASC.
func (d *Dispatcher) SortPendingCommands(commands []model.Command) []int {
	return sortPendingIndices(commands, func(c model.Command) sortKey {
		return sortKey{Status: c.Status, Priority: c.Priority, CreatedAt: c.CreatedAt, ID: c.ID}
	}, d.config.Queue.PriorityAgingSec)
}

// SortPendingNotifications sorts notifications by effective_priority ASC → created_at ASC → id ASC.
func (d *Dispatcher) SortPendingNotifications(notifications []model.Notification) []int {
	return sortPendingIndices(notifications, func(n model.Notification) sortKey {
		return sortKey{Status: n.Status, Priority: n.Priority, CreatedAt: n.CreatedAt, ID: n.ID}
	}, d.config.Queue.PriorityAgingSec)
}

// DispatchCommand dispatches a command to the planner agent.
func (d *Dispatcher) DispatchCommand(cmd *model.Command) error {
	exec, err := d.getExecutor()
	if err != nil {
		return fmt.Errorf("create executor: %w", err)
	}

	envelope := agent.BuildPlannerEnvelope(*cmd, cmd.LeaseEpoch, cmd.Attempts)

	result := exec.Execute(agent.ExecRequest{
		AgentID:    "planner",
		Message:    envelope,
		Mode:       agent.ModeDeliver,
		CommandID:  cmd.ID,
		LeaseEpoch: cmd.LeaseEpoch,
		Attempt:    cmd.Attempts,
	})

	if result.Error != nil {
		d.log(LogLevelError, "dispatch_command_failed id=%s error=%v retryable=%v",
			cmd.ID, result.Error, result.Retryable)
		return result.Error
	}

	d.log(LogLevelInfo, "dispatch_command_success id=%s epoch=%d", cmd.ID, cmd.LeaseEpoch)
	return nil
}

// DispatchTask dispatches a task to a worker agent.
func (d *Dispatcher) DispatchTask(task *model.Task, workerID string) error {
	// Pre-task quality gate check and record evaluation result
	var gateEvaluation *model.QualityGateEvaluation
	if d.shouldEvaluateQualityGates() && d.config.QualityGates.Enforcement.PreTaskCheck {
		evaluation, err := d.evaluatePreTaskGateWithResult(task, workerID)
		gateEvaluation = evaluation

		if err != nil {
			if d.config.QualityGates.Enforcement.FailureAction == "block" {
				d.log(LogLevelError, "dispatch_task_blocked_by_quality_gate id=%s worker=%s error=%v",
					task.ID, workerID, err)
				// Store evaluation result for later recording
				d.storeGateEvaluation(task.ID, gateEvaluation)
				return fmt.Errorf("quality gate check failed: %w", err)
			}
			// If action is "warn", just log the violation
			d.log(LogLevelWarn, "dispatch_task_quality_gate_violation id=%s worker=%s error=%v",
				task.ID, workerID, err)
		}
		// Store evaluation result for later recording
		d.storeGateEvaluation(task.ID, gateEvaluation)
	} else if !d.config.QualityGates.Enabled {
		// Record that gates were skipped
		gateEvaluation = &model.QualityGateEvaluation{
			Passed:        true,
			SkippedReason: "disabled",
			EvaluatedAt:   d.clock.Now().Format(time.RFC3339),
		}
		d.storeGateEvaluation(task.ID, gateEvaluation)
	} else if d.config.QualityGates.SkipGates {
		// Record that emergency mode was used
		gateEvaluation = &model.QualityGateEvaluation{
			Passed:        true,
			SkippedReason: "emergency_mode",
			EvaluatedAt:   d.clock.Now().Format(time.RFC3339),
		}
		d.storeGateEvaluation(task.ID, gateEvaluation)
	}

	exec, err := d.getExecutor()
	if err != nil {
		return fmt.Errorf("create executor: %w", err)
	}

	// Inject persona prompt into task content (best-effort, prepend)
	dispatchTask := *task
	// Sanitize user-supplied content to escape DATA boundary markers BEFORE
	// appending system-generated sections (skills, learnings) whose markers
	// must remain intact.
	dispatchTask.Content = agent.SanitizeUserContent(dispatchTask.Content)
	if task.PersonaHint != "" {
		if section, found := persona.FormatPersonaSectionWithFS(templates.FS, d.config.Personas, task.PersonaHint); !found {
			d.log(LogLevelWarn, "persona_not_found task=%s persona_hint=%s", task.ID, task.PersonaHint)
		} else if section != "" {
			dispatchTask.Content = section + dispatchTask.Content
			d.log(LogLevelDebug, "persona_injected task=%s persona=%s", task.ID, task.PersonaHint)
		}
	}

	// Inject skills into task content (between content and learnings)
	if d.config.Skills.Enabled && len(task.SkillRefs) > 0 {
		refs := task.SkillRefs
		maxRefs := d.config.Skills.EffectiveMaxRefsPerTask()
		if len(refs) > maxRefs {
			d.log(LogLevelWarn, "skill_refs_truncated task=%s total=%d max=%d", task.ID, len(refs), maxRefs)
			refs = refs[:maxRefs]
		}

		skillsDir := filepath.Join(d.maestroDir, "skills")
		policy := d.config.Skills.EffectiveMissingRefPolicy()
		var loaded []skill.SkillContent
		for _, ref := range refs {
			sc, err := skill.ReadSkillWithRole(skillsDir, ref, "worker")
			if err != nil {
				if policy == "error" {
					return fmt.Errorf("load skill ref %q for task %s: %w", ref, task.ID, err)
				}
				// warn policy: log and skip
				if errors.Is(err, os.ErrNotExist) {
					d.log(LogLevelWarn, "skill_ref_not_found task=%s ref=%s", task.ID, ref)
				} else {
					d.log(LogLevelWarn, "skill_read_failed task=%s ref=%s error=%v", task.ID, ref, err)
				}
				continue
			}
			loaded = append(loaded, sc)
		}

		if section := skill.FormatSkillSection(loaded, d.config.Skills.EffectiveMaxBodyChars()); section != "" {
			dispatchTask.Content = dispatchTask.Content + section
			d.log(LogLevelDebug, "skills_injected task=%s count=%d", task.ID, len(loaded))
		}
	}

	// Inject learnings into task content (read-only, best-effort)
	if d.config.Learnings.Enabled {
		lrns, err := learnings.ReadTopKLearnings(d.maestroDir, d.config.Learnings, d.clock.Now())
		if err != nil {
			d.log(LogLevelWarn, "learnings_read_failed task=%s error=%v", task.ID, err)
		} else if section := learnings.FormatLearningsSection(lrns); section != "" {
			dispatchTask.Content = dispatchTask.Content + section
			d.log(LogLevelDebug, "learnings_injected task=%s count=%d", task.ID, len(lrns))
		}
	}

	// Resolve working_dir for worktree-enabled commands (lazy creation)
	var workingDir string
	wm := d.getWorktreeManager()
	if wm != nil {
		wtPath, err := wm.GetWorkerPath(task.CommandID, workerID)
		if err != nil {
			// Worktree doesn't exist yet — lazily create for this worker
			if createErr := wm.EnsureWorkerWorktree(task.CommandID, workerID); createErr != nil {
				d.log(LogLevelError, "worktree_create_failed task=%s worker=%s error=%v",
					task.ID, workerID, createErr)
				return fmt.Errorf("worktree path resolution failed: %w", createErr)
			}
			wtPath, err = wm.GetWorkerPath(task.CommandID, workerID)
		}
		if err != nil {
			d.log(LogLevelError, "worktree_path_resolve_failed task=%s worker=%s error=%v",
				task.ID, workerID, err)
			return fmt.Errorf("worktree path resolution failed: %w", err)
		}
		workingDir = wtPath
	}

	envelope := agent.BuildWorkerEnvelope(dispatchTask, workerID, task.LeaseEpoch, task.Attempts)
	if workingDir != "" {
		envelope = fmt.Sprintf("%s\nworking_dir: %s", envelope, workingDir)
	}

	result := exec.Execute(agent.ExecRequest{
		AgentID:    workerID,
		Message:    envelope,
		Mode:       agent.ModeWithClear,
		TaskID:     task.ID,
		CommandID:  task.CommandID,
		LeaseEpoch: task.LeaseEpoch,
		Attempt:    task.Attempts,
		WorkingDir: workingDir,
	})

	if result.Error != nil {
		d.log(LogLevelError, "dispatch_task_failed id=%s worker=%s error=%v retryable=%v",
			task.ID, workerID, result.Error, result.Retryable)
		return result.Error
	}

	d.log(LogLevelInfo, "dispatch_task_success id=%s worker=%s epoch=%d",
		task.ID, workerID, task.LeaseEpoch)

	// Publish task_started event (non-blocking, best-effort)
	if bus := d.getEventBus(); bus != nil {
		bus.Publish(events.EventTaskStarted, map[string]interface{}{
			"task_id":    task.ID,
			"command_id": task.CommandID,
			"worker_id":  workerID,
			"epoch":      task.LeaseEpoch,
		})
	}

	return nil
}

// DispatchNotification dispatches a notification to the orchestrator agent.
func (d *Dispatcher) DispatchNotification(ntf *model.Notification) error {
	exec, err := d.getExecutor()
	if err != nil {
		return fmt.Errorf("create executor: %w", err)
	}

	envelope := agent.BuildOrchestratorNotificationEnvelope(ntf.CommandID, ntf.Type)

	result := exec.Execute(agent.ExecRequest{
		AgentID:    "orchestrator",
		Message:    envelope,
		Mode:       agent.ModeDeliver,
		CommandID:  ntf.CommandID,
		LeaseEpoch: ntf.LeaseEpoch,
		Attempt:    ntf.Attempts,
	})

	if result.Error != nil {
		d.log(LogLevelError, "dispatch_notification_failed id=%s error=%v retryable=%v",
			ntf.ID, result.Error, result.Retryable)
		return result.Error
	}

	d.log(LogLevelInfo, "dispatch_notification_success id=%s type=%s epoch=%d",
		ntf.ID, ntf.Type, ntf.LeaseEpoch)
	return nil
}

func (d *Dispatcher) log(level LogLevel, format string, args ...any) {
	d.dl.Logf(level, format, args...)
}

// shouldEvaluateQualityGates determines if quality gates should be evaluated
func (d *Dispatcher) shouldEvaluateQualityGates() bool {
	// Skip if quality gates are disabled
	if !d.config.QualityGates.Enabled {
		return false
	}

	// Skip if emergency mode is enabled (--skip-gates)
	if d.config.QualityGates.SkipGates {
		d.log(LogLevelInfo, "quality_gates_skipped reason=emergency_mode")
		return false
	}

	// Skip if quality gate daemon is not available
	if d.getQualityGate() == nil {
		d.log(LogLevelDebug, "quality_gates_skipped reason=daemon_not_available")
		return false
	}

	return true
}

// evaluatePreTaskGateWithResult evaluates quality gates before task execution and returns the result
func (d *Dispatcher) evaluatePreTaskGateWithResult(task *model.Task, workerID string) (*model.QualityGateEvaluation, error) {
	qg := d.getQualityGate()
	if qg == nil {
		return nil, nil
	}

	// NOTE: We do NOT emit TaskStartEvent here. The EventBus publish in
	// DispatchTask (after successful dispatch) triggers the subscription
	// in subscribeQualityGateEvents, which forwards the event to
	// QualityGateDaemon. Emitting here would cause duplicate delivery.

	// Perform synchronous evaluation
	context := map[string]interface{}{
		"task_id":    task.ID,
		"command_id": task.CommandID,
		"agent_id":   workerID,
		"priority":   task.Priority,
		"attempts":   task.Attempts,
	}

	// Use the evaluateGateWithResult method for synchronous evaluation
	result, err := qg.evaluateGateWithResult("pre_task", context)

	// Convert to model.QualityGateEvaluation
	evaluation := &model.QualityGateEvaluation{
		Passed:      result != nil && result.Passed,
		EvaluatedAt: d.clock.Now().Format(time.RFC3339),
	}

	if result != nil {
		evaluation.Action = string(result.Action)
		if len(result.FailedGates) > 0 {
			evaluation.FailedGates = make([]string, len(result.FailedGates))
			for i, gate := range result.FailedGates {
				evaluation.FailedGates[i] = gate
			}
		}
	}

	if err != nil {
		return evaluation, fmt.Errorf("evaluation failed: %w", err)
	}

	if result == nil {
		return evaluation, fmt.Errorf("evaluation returned nil result")
	}

	if !result.Passed {
		return evaluation, fmt.Errorf("quality gate check failed: %v", result.FailedGates)
	}

	return evaluation, nil
}

// storeGateEvaluation stores the gate evaluation for a task.
// Evicts oldest entries when the map exceeds maxGateEvaluations.
func (d *Dispatcher) storeGateEvaluation(taskID string, evaluation *model.QualityGateEvaluation) {
	if evaluation == nil {
		return
	}

	d.gateEvalMutex.Lock()
	defer d.gateEvalMutex.Unlock()
	d.gateEvaluations[taskID] = evaluation

	if len(d.gateEvaluations) > maxGateEvaluations {
		d.evictOldGateEvaluationsLocked()
	}
}

// evictOldGateEvaluationsLocked removes the oldest gate evaluation entries to bring
// the map back to maxGateEvaluations/2. Caller must hold gateEvalMutex.
func (d *Dispatcher) evictOldGateEvaluationsLocked() {
	type entry struct {
		taskID      string
		evaluatedAt time.Time
	}
	entries := make([]entry, 0, len(d.gateEvaluations))
	for id, eval := range d.gateEvaluations {
		t, err := time.Parse(time.RFC3339, eval.EvaluatedAt)
		if err != nil {
			// Malformed timestamp — evict immediately
			t = time.Time{}
		}
		entries = append(entries, entry{taskID: id, evaluatedAt: t})
	}

	// Sort oldest first
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].evaluatedAt.Before(entries[j].evaluatedAt)
	})

	// Remove oldest entries until we reach half the cap
	target := maxGateEvaluations / 2
	for i := 0; i < len(entries) && len(d.gateEvaluations) > target; i++ {
		delete(d.gateEvaluations, entries[i].taskID)
	}
}

// GetGateEvaluation retrieves the gate evaluation for a task
func (d *Dispatcher) GetGateEvaluation(taskID string) *model.QualityGateEvaluation {
	d.gateEvalMutex.RLock()
	defer d.gateEvalMutex.RUnlock()
	return d.gateEvaluations[taskID]
}
