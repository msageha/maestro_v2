package model

// DefaultPriority is the default priority value for commands, tasks, and notifications
// when no explicit priority is specified. Lower values indicate higher priority.
const DefaultPriority = 100

// CommandQueue はコマンドキューファイルの YAML 構造を表す。
// Daemon がコマンドのディスパッチとリース管理に使用する。
type CommandQueue struct {
	SchemaVersion int       `yaml:"schema_version"`
	FileType      string    `yaml:"file_type"`
	Commands      []Command `yaml:"commands"`
}

// Command はユーザーから投入された単一のコマンドを表す。
// Planner によるタスク分解とライフサイクル管理の単位となる。
type Command struct {
	ID                string   `yaml:"id"`
	DispatchID        string   `yaml:"dispatch_id,omitempty"`
	Content           string   `yaml:"content"`
	SkillRefs         []string `yaml:"skill_refs,omitempty"`
	Priority          int      `yaml:"priority"`
	Status            Status   `yaml:"status"`
	Attempts          int      `yaml:"attempts"`
	LastError         *string  `yaml:"last_error"`
	DeadLetteredAt    *string  `yaml:"dead_lettered_at"`
	DeadLetterReason  *string  `yaml:"dead_letter_reason"`
	LeaseOwner        *string  `yaml:"lease_owner"`
	LeaseExpiresAt    *string  `yaml:"lease_expires_at"`
	LeaseEpoch        int      `yaml:"lease_epoch"`
	CancelReason      *string  `yaml:"cancel_reason"`
	CancelRequestedAt *string  `yaml:"cancel_requested_at"`
	CancelRequestedBy *string  `yaml:"cancel_requested_by"`
	CreatedAt         string   `yaml:"created_at"`
	UpdatedAt         string   `yaml:"updated_at"`
}

// TaskQueue はタスクキューファイルの YAML 構造を表す。
// Daemon がタスクのディスパッチとリース管理に使用する。
type TaskQueue struct {
	SchemaVersion int    `yaml:"schema_version"`
	FileType      string `yaml:"file_type"`
	Tasks         []Task `yaml:"tasks"`
}

// DefinitionOfAbort はタスクの中断条件を定義する構造体。
type DefinitionOfAbort struct {
	MaxRepairCount            int      `yaml:"max_repair_count"`
	MaxWallClockSec           int      `yaml:"max_wall_clock_sec"`
	ExplicitFailureConditions []string `yaml:"explicit_failure_conditions"`
}

// DefaultDefinitionOfAbort は未指定時のデフォルト値を返す。
func DefaultDefinitionOfAbort() DefinitionOfAbort {
	return DefinitionOfAbort{
		MaxRepairCount:  3,
		MaxWallClockSec: 1800,
	}
}

// Task はコマンドから分解された単一の作業単位を表す。
// Worker に配信され、実行・結果報告のライフサイクルを持つ。
type Task struct {
	ID                 string             `yaml:"id"`
	DispatchID         string             `yaml:"dispatch_id,omitempty"`
	CommandID          string             `yaml:"command_id"`
	Purpose            string             `yaml:"purpose"`
	Content            string             `yaml:"content"`
	AcceptanceCriteria string             `yaml:"acceptance_criteria"`
	DefinitionOfDone   []string           `yaml:"definition_of_done,omitempty"`
	Constraints        []string           `yaml:"constraints"`
	BlockedBy          []string           `yaml:"blocked_by"`
	BloomLevel         int                `yaml:"bloom_level"`
	ToolsHint          []string           `yaml:"tools_hint,omitempty"`
	PersonaHint        string             `yaml:"persona_hint,omitempty"`
	SkillRefs          []string           `yaml:"skill_refs,omitempty"`
	ExpectedPaths      []string           `yaml:"expected_paths"`
	DefinitionOfAbort  *DefinitionOfAbort `yaml:"definition_of_abort"`
	Priority           int                `yaml:"priority"`
	Status             Status             `yaml:"status"`
	Attempts           int                `yaml:"attempts"`
	ExecutionRetries   int                `yaml:"execution_retries,omitempty"` // Number of actual retry executions (not dispatch attempts)
	OriginalTaskID     string             `yaml:"original_task_id,omitempty"`  // For tracking retry lineage
	NotBefore          *string            `yaml:"not_before,omitempty"`        // RFC3339 timestamp for cooldown
	// LastProgressEpoch records the lease epoch during which the daemon last
	// observed forward progress for this task (pane-activity VerdictActive
	// lease extension, or a confirmed-busy probe). The hang-release path
	// compares it against the current LeaseEpoch to distinguish "worker made
	// real progress and was then interrupted mid-stream" (progress-interrupt,
	// budget-exempt) from "worker wedged without ever producing output"
	// (consumes the task_dispatch budget as before). Issue #54.
	LastProgressEpoch int `yaml:"last_progress_epoch,omitempty"`
	// ProgressInterrupts counts hang-releases that followed observed progress
	// in the same epoch. These do NOT consume Attempts (the task_dispatch
	// dead-letter budget) up to retry.task_progress_interrupts; beyond the
	// cap the release falls back to the legacy Attempts accounting so a
	// pathological progress-then-idle loop still terminates. Issue #54.
	ProgressInterrupts int `yaml:"progress_interrupts,omitempty"`
	// ResumeAttempts counts continuation-nudge dispatches (resume without
	// /clear) consumed by this task. Bounded by retry.task_resume; beyond the
	// cap re-dispatch reverts to the full /clear envelope. Issue #55.
	ResumeAttempts int `yaml:"resume_attempts,omitempty"`
	// ResumeRequested marks that the most recent hang-release qualified for
	// an in-place resume (progress observed, task resume-eligible, resume
	// budget remaining). Consumed (cleared) by the next lease acquisition,
	// which turns that dispatch into a continuation nudge instead of a
	// /clear full re-delivery. Issue #55.
	ResumeRequested  bool    `yaml:"resume_requested,omitempty"`
	LastError        *string `yaml:"last_error"`
	DeadLetteredAt   *string `yaml:"dead_lettered_at"`
	DeadLetterReason *string `yaml:"dead_letter_reason"`
	LeaseOwner       *string `yaml:"lease_owner"`
	LeaseExpiresAt   *string `yaml:"lease_expires_at"`
	LeaseEpoch       int     `yaml:"lease_epoch"`
	InProgressAt     *string `yaml:"in_progress_at,omitempty"`
	CreatedAt        string  `yaml:"created_at"`
	UpdatedAt        string  `yaml:"updated_at"`

	// Runtime selection is retained for schema compatibility. Managed roles
	// currently accept claude-code only; config/launcher reject codex/gemini.
	Runtime string `yaml:"runtime,omitempty" json:"runtime,omitempty"`
	// Runtime-specific model override.
	ModelOverride string `yaml:"model_override,omitempty" json:"model_override,omitempty"`
	// C-6/C-8: Complexity level (simple|standard|complex|critical)
	ComplexityLevel string `yaml:"complexity_level,omitempty" json:"complexity_level,omitempty"`
	// ABGroupID marks this task as an A/B candidate belonging to the given
	// CandidateGroup (state.candidate_groups). Empty for normal tasks. The
	// daemon uses it to route the task into a candidate-exclusive worktree
	// and to apply the pre-selection barrier. See docs/design/
	// ab_candidate_selection.md.
	ABGroupID string `yaml:"ab_group_id,omitempty" json:"ab_group_id,omitempty"`
	// RunOnMain instructs the dispatcher to run this task in the main working
	// directory instead of the worker's worktree. Use for read-only verification
	// tasks that must evaluate the merged state on the main branch.
	RunOnMain bool `yaml:"run_on_main,omitempty" json:"run_on_main,omitempty"`
	// RunOnIntegration instructs the dispatcher to run this task in the
	// integration worktree for the associated command. Use for publish_conflict
	// resolution tasks that must operate directly on the integration branch to
	// resolve forward-merge conflicts before retry-publish can succeed.
	RunOnIntegration bool `yaml:"run_on_integration,omitempty" json:"run_on_integration,omitempty"`
	// ResumeHint optionally overrides the resume-eligibility policy for this
	// task ("allow" / "deny"). When empty, the default policy applies: resume
	// is allowed for worker-worktree tasks (mutations are isolated to the
	// task-private worktree and reconciled at merge/verify time — the same
	// exposure the /clear full re-dispatch already has, since neither path
	// discards the worktree) and RunOnMain tasks (read-only enforced by the
	// @run_on_main hook), and denied for RunOnIntegration tasks (they mutate
	// the shared integration tree, where a double-executed tool call is not
	// contained). Planner-settable per task. Issue #55.
	ResumeHint string `yaml:"resume_hint,omitempty" json:"resume_hint,omitempty"`
	// OperationType classifies the task for admission control. Permitted
	// values are OperationTypeVerify / Repair, or empty for normal
	// (unconstrained) tasks. Classification is deterministic and explicit:
	// when this field is empty the admission controller treats the task as
	// OpUnknown (i.e. unconstrained), regardless of any keywords that happen
	// to appear in Purpose. Earlier revisions inferred the type from
	// Purpose-substring matches, but that heuristic was removed because it
	// misclassified user tasks like "Repair broken auth flow" into the
	// admission-controlled bucket — the contract is locked in by
	// internal/daemon/admission/controller_test.go::TestClassifyTask_PurposeIgnored.
	// Planner / retry handler should therefore populate this field explicitly
	// for verify / repair tasks.
	OperationType string `yaml:"operation_type,omitempty" json:"operation_type,omitempty"`
}

// Operation type values used by Task.OperationType. Keep these aligned with
// the admission Controller's OpType.String() output so YAML round-trips and
// classifier lookups stay consistent.
const (
	OperationTypeVerify = "verify"
	OperationTypeRepair = "repair"
)

// Resume hint values used by Task.ResumeHint (issue #55).
const (
	ResumeHintAllow = "allow"
	ResumeHintDeny  = "deny"
)

// ResumeEligible reports whether this task may be recovered via an in-place
// continuation nudge (resume) instead of a /clear full re-dispatch after a
// progress-interrupt hang-release. The explicit ResumeHint wins; otherwise
// RunOnIntegration tasks are denied because they mutate the shared
// integration worktree where double-executed tool calls are not contained
// (see the ResumeHint field comment for the full policy rationale).
func (t *Task) ResumeEligible() bool {
	switch t.ResumeHint {
	case ResumeHintAllow:
		return true
	case ResumeHintDeny:
		return false
	}
	return !t.RunOnIntegration
}

// GetDoneConditions は完了条件を返す。
// DefinitionOfDone が設定されている場合はそちらを優先し、
// 未設定の場合は AcceptanceCriteria を単一要素のスライスとして返す。
// どちらも未設定の場合は空スライスを返す（nil は返さない）。
func (t *Task) GetDoneConditions() []string {
	if len(t.DefinitionOfDone) > 0 {
		return t.DefinitionOfDone
	}
	if t.AcceptanceCriteria != "" {
		return []string{t.AcceptanceCriteria}
	}
	return []string{}
}

// NotificationQueue は通知キューファイルの YAML 構造を表す。
// タスク・コマンド結果の通知配信をリース方式で管理する。
type NotificationQueue struct {
	SchemaVersion int            `yaml:"schema_version"`
	FileType      string         `yaml:"file_type"`
	Notifications []Notification `yaml:"notifications"`
}

// Notification はタスクまたはコマンドの結果に対する単一の通知エントリを表す。
// Orchestrator / Planner への結果配信に使用される。
type Notification struct {
	ID               string           `yaml:"id"`
	CommandID        string           `yaml:"command_id"`
	Type             NotificationType `yaml:"type"`
	SourceResultID   string           `yaml:"source_result_id"`
	Content          string           `yaml:"content"`
	Priority         int              `yaml:"priority"`
	Status           Status           `yaml:"status"`
	Attempts         int              `yaml:"attempts"`
	LastError        *string          `yaml:"last_error"`
	DeadLetteredAt   *string          `yaml:"dead_lettered_at"`
	DeadLetterReason *string          `yaml:"dead_letter_reason"`
	LeaseOwner       *string          `yaml:"lease_owner"`
	LeaseExpiresAt   *string          `yaml:"lease_expires_at"`
	LeaseEpoch       int              `yaml:"lease_epoch"`
	CreatedAt        string           `yaml:"created_at"`
	UpdatedAt        string           `yaml:"updated_at"`
}

// PlannerSignalQueue は Planner 向けシグナルキューファイルの YAML 構造を表す。
// フェーズ完了やコンフリクト検知などのイベントを Planner に伝達する。
type PlannerSignalQueue struct {
	SchemaVersion int             `yaml:"schema_version"`
	FileType      string          `yaml:"file_type"`
	Signals       []PlannerSignal `yaml:"signals"`
}

// PlannerSignal は Planner に送信される単一のシグナルを表す。
// フェーズ完了、コミット失敗、マージコンフリクトなどの種別を持ち、
// Planner がリカバリアクションを判断するための構造化情報を格納する。
type PlannerSignal struct {
	Kind          string  `yaml:"kind"`
	CommandID     string  `yaml:"command_id"`
	PhaseID       string  `yaml:"phase_id"`
	PhaseName     string  `yaml:"phase_name"`
	Message       string  `yaml:"message"`
	Attempts      int     `yaml:"attempts"`
	CreatedAt     string  `yaml:"created_at"`
	UpdatedAt     string  `yaml:"updated_at"`
	LastAttemptAt *string `yaml:"last_attempt_at"`
	NextAttemptAt *string `yaml:"next_attempt_at"`
	LastError     *string `yaml:"last_error"`
	// WorkerID disambiguates per-worker signals (e.g. commit_failed) so multiple
	// workers in the same phase each retain a distinct entry. Empty for
	// phase-level signals — preserves the legacy dedup key.
	WorkerID string `yaml:"worker_id,omitempty"`
	// Reason is a structured machine-readable classification for the signal
	// (e.g. "all_files_filtered", "policy_violation:max_files_exceeded",
	// "generic:<message>"). Currently populated for commit_failed signals so
	// the planner can take recovery action without parsing free-form text.
	Reason string `yaml:"reason,omitempty"`
	// ConflictBaseRef / ConflictOursRef / ConflictTheirsRef are populated for
	// merge_conflict signals to give planners structured access to the
	// underlying refs (base = common ancestor, ours = integration side,
	// theirs = worker side). Empty for non-conflict signals. Added in MVP-1;
	// the legacy free-form Message field is preserved for backward compat.
	ConflictBaseRef   string `yaml:"conflict_base_ref,omitempty"`
	ConflictOursRef   string `yaml:"conflict_ours_ref,omitempty"`
	ConflictTheirsRef string `yaml:"conflict_theirs_ref,omitempty"`
	// ConflictFiles lists the files reported in conflict by git for
	// merge_conflict signals. Empty for non-conflict signals.
	ConflictFiles []string `yaml:"conflict_files,omitempty"`

	// Conflict-resolution lifecycle fields (Phase 1 SSOT, additive).
	// ResolutionState tracks the current resolver state machine for this
	// signal: pending|dispatched|resolving|failed. Empty for signals that
	// have not entered the resolver pipeline.
	ResolutionState string `yaml:"resolution_state,omitempty"`
	// ResolveAttempt is the number of resolver commit attempts that have been
	// made for this signal. Used by attempt-limit logic.
	ResolveAttempt int `yaml:"resolve_attempt,omitempty"`
	// LastResolutionError captures the most recent resolver error message for
	// observability and planner backoff decisions.
	LastResolutionError string `yaml:"last_resolution_error,omitempty"`
	// ConflictGeneration is a deterministic CAS token computed from the
	// integration HEAD, the worker branch SHA, the phase ID and the worker ID
	// at the moment the conflict was detected. The resolver must echo this
	// value back to confirm it is acting on the same generation.
	ConflictGeneration string `yaml:"conflict_generation,omitempty"`
	// ConflictType classifies the merge_conflict signal:
	//   "task_merge_conflict" — worker → integration merge conflict (default/legacy)
	//   "publish_conflict"    — integration → base (main) publish merge conflict
	// Empty for non-conflict signals or legacy signals (treated as task_merge_conflict).
	ConflictType string `yaml:"conflict_type,omitempty"`
}
