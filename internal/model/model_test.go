package model

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestConfigMarshalUnmarshal(t *testing.T) {
	cfg := Config{
		Project: ProjectConfig{
			Name:        "test-project",
			Description: "A test project",
		},
		Maestro: MaestroConfig{
			Version:     "2.0.0",
			Created:     "2026-02-23T10:00:00+09:00",
			ProjectRoot: "/tmp/test",
		},
		Agents: AgentsConfig{
			Orchestrator: AgentConfig{ID: "orchestrator", Model: "opus"},
			Planner:      AgentConfig{ID: "planner", Model: "opus"},
			Workers: WorkerConfig{
				Count:        4,
				DefaultModel: "sonnet",
				Models:       map[string]string{"worker3": "opus"},
				Boost:        false,
			},
		},
		Continuous: ContinuousConfig{
			Enabled:        false,
			MaxIterations:  10,
			PauseOnFailure: true,
		},
		Watcher: WatcherConfig{
			DebounceSec:         0.3,
			ScanIntervalSec:     60,
			DispatchLeaseSec:    120,
			MaxInProgressMin:    30,
			BusyCheckInterval:   2,
			BusyCheckMaxRetries: 30,
			BusyPatterns:        "Working|Thinking|Planning|Sending|Searching",
			IdleStableSec:       5,
			CooldownAfterClear:  3,
			NotifyLeaseSec:      120,
		},
		Retry: RetryConfig{
			CommandDispatch:                  5,
			TaskDispatch:                     5,
			OrchestratorNotificationDispatch: 10,
		},
		Queue: QueueConfig{PriorityAgingSec: 300},
		Limits: LimitsConfig{
			MaxPendingCommands:       20,
			MaxPendingTasksPerWorker: 10,
			MaxEntryContentBytes:     65536,
			MaxYAMLFileBytes:         5242880,
		},
		Daemon:  DaemonConfig{ShutdownTimeoutSec: 90},
		Logging: LoggingConfig{Level: "info"},
	}

	data, err := yaml.Marshal(&cfg)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded Config
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded.Project.Name != cfg.Project.Name {
		t.Errorf("project.name: got %q, want %q", decoded.Project.Name, cfg.Project.Name)
	}
	if decoded.Agents.Workers.Count != cfg.Agents.Workers.Count {
		t.Errorf("workers.count: got %d, want %d", decoded.Agents.Workers.Count, cfg.Agents.Workers.Count)
	}
	if decoded.Agents.Workers.Models["worker3"] != "opus" {
		t.Errorf("workers.models.worker3: got %q, want %q", decoded.Agents.Workers.Models["worker3"], "opus")
	}
	if decoded.Watcher.DebounceSec != 0.3 {
		t.Errorf("watcher.debounce_sec: got %f, want %f", decoded.Watcher.DebounceSec, 0.3)
	}
	if decoded.Limits.MaxEntryContentBytes != 65536 {
		t.Errorf("limits.max_entry_content_bytes: got %d, want %d", decoded.Limits.MaxEntryContentBytes, 65536)
	}
}

func TestCommandQueueMarshalUnmarshal(t *testing.T) {
	q := CommandQueue{
		SchemaVersion: 1,
		FileType:      "queue_command",
		Commands: []Command{
			{
				ID:         "cmd_1771722000_a3f2b7c1",
				Content:    "implement login API",
				Priority:   100,
				Status:     StatusPending,
				Attempts:   0,
				LeaseEpoch: 0,
				CreatedAt:  "2026-02-23T10:00:00+09:00",
				UpdatedAt:  "2026-02-23T10:00:00+09:00",
			},
		},
	}

	data, err := yaml.Marshal(&q)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded CommandQueue
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if len(decoded.Commands) != 1 {
		t.Fatalf("expected 1 command, got %d", len(decoded.Commands))
	}
	cmd := decoded.Commands[0]
	if cmd.ID != "cmd_1771722000_a3f2b7c1" {
		t.Errorf("command.id: got %q", cmd.ID)
	}
	if cmd.Status != StatusPending {
		t.Errorf("command.status: got %q", cmd.Status)
	}
	if cmd.LeaseOwner != nil {
		t.Errorf("command.lease_owner: expected nil, got %v", cmd.LeaseOwner)
	}
}

func TestTaskQueueMarshalUnmarshal(t *testing.T) {
	q := TaskQueue{
		SchemaVersion: 1,
		FileType:      "queue_task",
		Tasks: []Task{
			{
				ID:                 "task_1771722060_b7c1d4e9",
				CommandID:          "cmd_1771722000_a3f2b7c1",
				Purpose:            "Implement login endpoint",
				Content:            "Create POST /api/login",
				AcceptanceCriteria: "Tests pass",
				Constraints:        []string{"Use JWT"},
				BlockedBy:          []string{},
				BloomLevel:         3,
				ToolsHint:          []string{"context7"},
				Priority:           100,
				Status:             StatusPending,
				Attempts:           0,
				LeaseEpoch:         0,
				CreatedAt:          "2026-02-23T10:00:00+09:00",
				UpdatedAt:          "2026-02-23T10:00:00+09:00",
			},
		},
	}

	data, err := yaml.Marshal(&q)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded TaskQueue
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if len(decoded.Tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(decoded.Tasks))
	}
	task := decoded.Tasks[0]
	if task.BloomLevel != 3 {
		t.Errorf("task.bloom_level: got %d", task.BloomLevel)
	}
	if len(task.ToolsHint) != 1 || task.ToolsHint[0] != "context7" {
		t.Errorf("task.tools_hint: got %v", task.ToolsHint)
	}
}

func TestNotificationQueueMarshalUnmarshal(t *testing.T) {
	q := NotificationQueue{
		SchemaVersion: 1,
		FileType:      "queue_notification",
		Notifications: []Notification{
			{
				ID:             "ntf_1771722600_d4e9f0a2",
				CommandID:      "cmd_1771722000_a3f2b7c1",
				Type:           NotificationTypeCommandCompleted,
				SourceResultID: "res_1771722600_f1a2b3c4",
				Content:        "Command completed successfully",
				Priority:       100,
				Status:         StatusPending,
				Attempts:       0,
				LeaseEpoch:     0,
				CreatedAt:      "2026-02-23T10:00:00+09:00",
				UpdatedAt:      "2026-02-23T10:00:00+09:00",
			},
		},
	}

	data, err := yaml.Marshal(&q)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded NotificationQueue
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if len(decoded.Notifications) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(decoded.Notifications))
	}
	ntf := decoded.Notifications[0]
	if ntf.Type != NotificationTypeCommandCompleted {
		t.Errorf("notification.type: got %q, want %q", ntf.Type, NotificationTypeCommandCompleted)
	}
	if ntf.SourceResultID != "res_1771722600_f1a2b3c4" {
		t.Errorf("notification.source_result_id: got %q", ntf.SourceResultID)
	}
}

func TestTaskResultFileMarshalUnmarshal(t *testing.T) {
	f := TaskResultFile{
		SchemaVersion: 1,
		FileType:      "result_task",
		Results: []TaskResult{
			{
				ID:                     "res_1771722300_e5f0c3d8",
				TaskID:                 "task_1771722060_b7c1d4e9",
				CommandID:              "cmd_1771722000_a3f2b7c1",
				Status:                 StatusCompleted,
				Summary:                "Login API implemented",
				FilesChanged:           []string{"src/api/login.ts", "tests/api/login.test.ts"},
				PartialChangesPossible: false,
				RetrySafe:              true,
				Notified:               false,
				NotifyAttempts:         0,
				CreatedAt:              "2026-02-23T10:05:00+09:00",
			},
		},
	}

	data, err := yaml.Marshal(&f)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded TaskResultFile
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if len(decoded.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(decoded.Results))
	}
	r := decoded.Results[0]
	if r.Status != StatusCompleted {
		t.Errorf("result.status: got %q", r.Status)
	}
	if len(r.FilesChanged) != 2 {
		t.Errorf("result.files_changed: expected 2, got %d", len(r.FilesChanged))
	}
}

func TestCommandResultFileMarshalUnmarshal(t *testing.T) {
	f := CommandResultFile{
		SchemaVersion: 1,
		FileType:      "result_command",
		Results: []CommandResult{
			{
				ID:        "res_1771722600_f1a2b3c4",
				CommandID: "cmd_1771722000_a3f2b7c1",
				Status:    StatusCompleted,
				Summary:   "All tasks completed",
				Tasks: []CommandResultTask{
					{
						TaskID:  "task_1771722060_b7c1d4e9",
						Worker:  "worker3",
						Status:  StatusCompleted,
						Summary: "Login API done",
					},
				},
				Notified:       false,
				NotifyAttempts: 0,
				CreatedAt:      "2026-02-23T10:10:00+09:00",
			},
		},
	}

	data, err := yaml.Marshal(&f)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded CommandResultFile
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if len(decoded.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(decoded.Results))
	}
	r := decoded.Results[0]
	if len(r.Tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(r.Tasks))
	}
	if r.Tasks[0].Worker != "worker3" {
		t.Errorf("result.tasks[0].worker: got %q", r.Tasks[0].Worker)
	}
}

func TestCommandStateMarshalUnmarshal(t *testing.T) {
	commitTaskID := "task_1771722180_commit01"
	s := CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     "cmd_1771722000_a3f2b7c1",
		PlanVersion:   1,
		PlanStatus:    PlanStatusSealed,
		CompletionPolicy: CompletionPolicy{
			Mode:                    "all_required_completed",
			AllowDynamicTasks:       false,
			OnRequiredFailed:        "fail_command",
			OnRequiredCancelled:     "cancel_command",
			OnOptionalFailed:        "ignore",
			DependencyFailurePolicy: "cancel_dependents",
		},
		Cancel: CancelState{
			Requested: false,
		},
		ExpectedTaskCount: 2,
		RequiredTaskIDs:   []string{"task_1771722060_b7c1d4e9", "task_1771722120_c2d3e5f0"},
		OptionalTaskIDs:   []string{},
		TaskDependencies: map[string][]string{
			"task_1771722060_b7c1d4e9": {},
			"task_1771722120_c2d3e5f0": {"task_1771722060_b7c1d4e9"},
		},
		TaskStates: map[string]Status{
			"task_1771722060_b7c1d4e9": StatusPending,
			"task_1771722120_c2d3e5f0": StatusPending,
		},
		CancelledReasons:   map[string]string{},
		AppliedResultIDs:   map[string]string{},
		SystemCommitTaskID: &commitTaskID,
		RetryLineage:       map[string]string{},
		CreatedAt:          "2026-02-23T10:00:00+09:00",
		UpdatedAt:          "2026-02-23T10:00:00+09:00",
	}

	data, err := yaml.Marshal(&s)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded CommandState
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded.PlanStatus != PlanStatusSealed {
		t.Errorf("plan_status: got %q", decoded.PlanStatus)
	}
	if decoded.ExpectedTaskCount != 2 {
		t.Errorf("expected_task_count: got %d", decoded.ExpectedTaskCount)
	}
	deps := decoded.TaskDependencies["task_1771722120_c2d3e5f0"]
	if len(deps) != 1 || deps[0] != "task_1771722060_b7c1d4e9" {
		t.Errorf("task_dependencies: got %v", deps)
	}
	if decoded.SystemCommitTaskID == nil || *decoded.SystemCommitTaskID != commitTaskID {
		t.Errorf("system_commit_task_id: got %v", decoded.SystemCommitTaskID)
	}
}

func TestCommandStateWithPhasesMarshalUnmarshal(t *testing.T) {
	s := CommandState{
		SchemaVersion: 1,
		FileType:      "state_command",
		CommandID:     "cmd_1771722000_a3f2b7c1",
		PlanVersion:   1,
		PlanStatus:    PlanStatusSealed,
		CompletionPolicy: CompletionPolicy{
			Mode:                    "all_required_completed",
			AllowDynamicTasks:       false,
			OnRequiredFailed:        "fail_command",
			OnRequiredCancelled:     "cancel_command",
			OnOptionalFailed:        "ignore",
			DependencyFailurePolicy: "cancel_dependents",
		},
		Cancel: CancelState{Requested: false},
		Phases: []Phase{
			{
				PhaseID:         "phase_1771722000_c3d4e5f6",
				Name:            "research",
				Type:            "concrete",
				Status:          PhaseStatusActive,
				DependsOnPhases: []string{},
				TaskIDs:         []string{"task_1771722060_b7c1d4e9"},
			},
			{
				PhaseID:         "phase_1771722000_d4e5f6a7",
				Name:            "implementation",
				Type:            "deferred",
				Status:          PhaseStatusPending,
				DependsOnPhases: []string{"research"},
				TaskIDs:         []string{},
				Constraints: &PhaseConstraints{
					MaxTasks:           6,
					AllowedBloomLevels: []int{1, 2, 3, 4, 5, 6},
					TimeoutMinutes:     60,
				},
			},
		},
		TaskStates:       map[string]Status{"task_1771722060_b7c1d4e9": StatusPending},
		CancelledReasons: map[string]string{},
		AppliedResultIDs: map[string]string{},
		RetryLineage:     map[string]string{},
		CreatedAt:        "2026-02-23T10:00:00+09:00",
		UpdatedAt:        "2026-02-23T10:00:00+09:00",
	}

	data, err := yaml.Marshal(&s)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded CommandState
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if len(decoded.Phases) != 2 {
		t.Fatalf("expected 2 phases, got %d", len(decoded.Phases))
	}
	if decoded.Phases[0].Name != "research" {
		t.Errorf("phase[0].name: got %q", decoded.Phases[0].Name)
	}
	if decoded.Phases[1].Constraints == nil {
		t.Fatal("phase[1].constraints: expected non-nil")
	}
	if decoded.Phases[1].Constraints.MaxTasks != 6 {
		t.Errorf("phase[1].constraints.max_tasks: got %d", decoded.Phases[1].Constraints.MaxTasks)
	}
	if decoded.Phases[1].Constraints.TimeoutMinutes != 60 {
		t.Errorf("phase[1].constraints.timeout_minutes: got %d", decoded.Phases[1].Constraints.TimeoutMinutes)
	}
}

func TestMetricsMarshalUnmarshal(t *testing.T) {
	m := Metrics{
		SchemaVersion: 1,
		FileType:      "state_metrics",
		QueueDepth: QueueDepth{
			Planner:      0,
			Orchestrator: 0,
			Workers:      map[string]int{"worker1": 0, "worker2": 0},
		},
		Counters: MetricsCounters{},
	}

	data, err := yaml.Marshal(&m)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded Metrics
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded.QueueDepth.Workers["worker1"] != 0 {
		t.Errorf("queue_depth.workers.worker1: got %d", decoded.QueueDepth.Workers["worker1"])
	}
}

// --- Effective* method boundary value tests ---

func TestLimitsConfig_EffectiveMaxDeadLetterArchiveFiles(t *testing.T) {
	tests := []struct {
		name  string
		value int
		want  int
	}{
		{"zero returns default 100", 0, 100},
		{"negative returns default 100", -1, 100},
		{"positive returns configured", 50, 50},
		{"one returns configured", 1, 1},
		{"large value returns configured", 999, 999},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := LimitsConfig{MaxDeadLetterArchiveFiles: tt.value}
			if got := l.EffectiveMaxDeadLetterArchiveFiles(); got != tt.want {
				t.Errorf("EffectiveMaxDeadLetterArchiveFiles() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestLimitsConfig_EffectiveMaxQuarantineFiles(t *testing.T) {
	tests := []struct {
		name  string
		value int
		want  int
	}{
		{"zero returns default 100", 0, 100},
		{"negative returns default 100", -1, 100},
		{"positive returns configured", 25, 25},
		{"one returns configured", 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := LimitsConfig{MaxQuarantineFiles: tt.value}
			if got := l.EffectiveMaxQuarantineFiles(); got != tt.want {
				t.Errorf("EffectiveMaxQuarantineFiles() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestCircuitBreakerConfig_EffectiveMaxConsecutiveFailures(t *testing.T) {
	tests := []struct {
		name  string
		value int
		want  int
	}{
		{"zero returns default 3", 0, 3},
		{"negative returns default 3", -1, 3},
		{"positive returns configured", 10, 10},
		{"one returns configured", 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := CircuitBreakerConfig{MaxConsecutiveFailures: tt.value}
			if got := c.EffectiveMaxConsecutiveFailures(); got != tt.want {
				t.Errorf("EffectiveMaxConsecutiveFailures() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestCircuitBreakerConfig_EffectiveProgressTimeoutMinutes(t *testing.T) {
	tests := []struct {
		name  string
		value int
		want  int
	}{
		{"zero returns default", 0, 30},
		{"positive returns configured", 30, 30},
		{"custom value", 60, 60},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := CircuitBreakerConfig{ProgressTimeoutMinutes: tt.value}
			if got := c.EffectiveProgressTimeoutMinutes(); got != tt.want {
				t.Errorf("EffectiveProgressTimeoutMinutes() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestLearningsConfig_EffectiveMaxEntries(t *testing.T) {
	tests := []struct {
		name  string
		value int
		want  int
	}{
		{"zero returns default 100", 0, 100},
		{"negative returns default 100", -1, 100},
		{"positive returns configured", 200, 200},
		{"one returns configured", 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := LearningsConfig{MaxEntries: tt.value}
			if got := l.EffectiveMaxEntries(); got != tt.want {
				t.Errorf("EffectiveMaxEntries() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestLearningsConfig_EffectiveMaxContentLength(t *testing.T) {
	tests := []struct {
		name  string
		value int
		want  int
	}{
		{"zero returns default 500", 0, 500},
		{"negative returns default 500", -1, 500},
		{"positive returns configured", 1000, 1000},
		{"one returns configured", 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := LearningsConfig{MaxContentLength: tt.value}
			if got := l.EffectiveMaxContentLength(); got != tt.want {
				t.Errorf("EffectiveMaxContentLength() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestLearningsConfig_EffectiveInjectCount(t *testing.T) {
	tests := []struct {
		name  string
		value int
		want  int
	}{
		{"zero returns default 5", 0, 5},
		{"negative returns default 5", -1, 5},
		{"positive returns configured", 10, 10},
		{"one returns configured", 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := LearningsConfig{InjectCount: tt.value}
			if got := l.EffectiveInjectCount(); got != tt.want {
				t.Errorf("EffectiveInjectCount() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestLearningsConfig_EffectiveTTLHours(t *testing.T) {
	tests := []struct {
		name  string
		value int
		want  int
	}{
		{"zero returns default", 0, 72},
		{"positive returns configured", 72, 72},
		{"custom value", 24, 24},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := LearningsConfig{TTLHours: tt.value}
			if got := l.EffectiveTTLHours(); got != tt.want {
				t.Errorf("EffectiveTTLHours() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestVerificationConfig_EffectiveTimeoutSeconds(t *testing.T) {
	tests := []struct {
		name  string
		value int
		want  int
	}{
		{"zero returns default 300", 0, 300},
		{"negative returns default 300", -1, 300},
		{"positive returns configured", 600, 600},
		{"one returns configured", 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := VerificationConfig{TimeoutSeconds: tt.value}
			if got := v.EffectiveTimeoutSeconds(); got != tt.want {
				t.Errorf("EffectiveTimeoutSeconds() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestVerificationConfig_EffectiveMaxRetries(t *testing.T) {
	tests := []struct {
		name  string
		value int
		want  int
	}{
		{"zero means no retry", 0, 0},
		{"positive returns configured", 3, 3},
		{"one returns configured", 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := VerificationConfig{MaxRetries: tt.value}
			if got := v.EffectiveMaxRetries(); got != tt.want {
				t.Errorf("EffectiveMaxRetries() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestWorktreeConfig_EffectiveBaseBranch(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{"empty returns default main", "", "main"},
		{"custom returns configured", "develop", "develop"},
		{"master returns configured", "master", "master"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := WorktreeConfig{BaseBranch: tt.value}
			if got := w.EffectiveBaseBranch(); got != tt.want {
				t.Errorf("EffectiveBaseBranch() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWorktreeConfig_EffectivePathPrefix(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{"empty returns default", "", ".maestro/worktrees"},
		{"custom returns configured", "/tmp/worktrees", "/tmp/worktrees"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := WorktreeConfig{PathPrefix: tt.value}
			if got := w.EffectivePathPrefix(); got != tt.want {
				t.Errorf("EffectivePathPrefix() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWorktreeConfig_EffectiveMergeStrategy(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{"empty returns default ort", "", "ort"},
		{"custom returns configured", "recursive", "recursive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := WorktreeConfig{MergeStrategy: tt.value}
			if got := w.EffectiveMergeStrategy(); got != tt.want {
				t.Errorf("EffectiveMergeStrategy() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWorktreeGCConfig_EffectiveTTLHours(t *testing.T) {
	tests := []struct {
		name  string
		value int
		want  int
	}{
		{"zero returns default 24", 0, 24},
		{"negative returns default 24", -1, 24},
		{"positive returns configured", 48, 48},
		{"one returns configured", 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gc := WorktreeGCConfig{TTLHours: tt.value}
			if got := gc.EffectiveTTLHours(); got != tt.want {
				t.Errorf("EffectiveTTLHours() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestWorktreeConfig_EffectiveGitTimeout(t *testing.T) {
	tests := []struct {
		name  string
		value int
		want  int
	}{
		{"zero returns default 120", 0, 120},
		{"negative returns default 120", -1, 120},
		{"positive returns configured", 60, 60},
		{"one returns configured", 1, 1},
		{"large value returns configured", 300, 300},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := WorktreeConfig{GitTimeoutSec: tt.value}
			if got := w.EffectiveGitTimeout(); got != tt.want {
				t.Errorf("EffectiveGitTimeout() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestWorktreeConfig_GitTimeoutSecYAMLRoundTrip(t *testing.T) {
	yamlData := []byte(`
worktree:
  enabled: true
  git_timeout_sec: 60
`)
	var cfg Config
	if err := yaml.Unmarshal(yamlData, &cfg); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if cfg.Worktree.GitTimeoutSec != 60 {
		t.Errorf("GitTimeoutSec: got %d, want 60", cfg.Worktree.GitTimeoutSec)
	}
	if cfg.Worktree.EffectiveGitTimeout() != 60 {
		t.Errorf("EffectiveGitTimeout(): got %d, want 60", cfg.Worktree.EffectiveGitTimeout())
	}

	// Round-trip
	data, err := yaml.Marshal(&cfg)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	var decoded Config
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal round-trip failed: %v", err)
	}
	if decoded.Worktree.GitTimeoutSec != 60 {
		t.Errorf("round-trip GitTimeoutSec: got %d, want 60", decoded.Worktree.GitTimeoutSec)
	}
}

func TestWorktreeGCConfig_EffectiveMaxWorktrees(t *testing.T) {
	tests := []struct {
		name  string
		value int
		want  int
	}{
		{"zero returns default 32", 0, 32},
		{"negative returns default 32", -1, 32},
		{"positive returns configured", 16, 16},
		{"one returns configured", 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gc := WorktreeGCConfig{MaxWorktrees: tt.value}
			if got := gc.EffectiveMaxWorktrees(); got != tt.want {
				t.Errorf("EffectiveMaxWorktrees() = %d, want %d", got, tt.want)
			}
		})
	}
}

// --- CommitPolicyConfig Tests ---

func TestCommitPolicyConfig_EffectiveMaxFiles(t *testing.T) {
	tests := []struct {
		name  string
		value int
		want  int
	}{
		{"zero returns default 30", 0, 30},
		{"negative returns default 30", -1, 30},
		{"positive returns configured", 50, 50},
		{"one returns configured", 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := CommitPolicyConfig{MaxFiles: tt.value}
			if got := c.EffectiveMaxFiles(); got != tt.want {
				t.Errorf("EffectiveMaxFiles() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestCommitPolicyConfig_RequireGitignore(t *testing.T) {
	tests := []struct {
		name   string
		config CommitPolicyConfig
		want   bool
	}{
		{"zero-valued struct is false", CommitPolicyConfig{}, false},
		{"explicitly true", CommitPolicyConfig{RequireGitignore: true}, true},
		{"explicitly false", CommitPolicyConfig{MaxFiles: 30, RequireGitignore: false}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.config.RequireGitignore; got != tt.want {
				t.Errorf("RequireGitignore = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCommitPolicyConfig_MessagePattern(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{"empty means no check", "", ""},
		{"custom pattern", `^\[maestro\]\s`, `^\[maestro\]\s`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := CommitPolicyConfig{MessagePattern: tt.value}
			if got := c.MessagePattern; got != tt.want {
				t.Errorf("MessagePattern = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCommitPolicyConfig_YAMLRoundTrip(t *testing.T) {
	yamlData := []byte(`
worktree:
  enabled: true
  commit_policy:
    max_files: 50
    require_gitignore: true
    message_pattern: "^\\[maestro\\]\\s"
`)
	var cfg Config
	if err := yaml.Unmarshal(yamlData, &cfg); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if cfg.Worktree.CommitPolicy.MaxFiles != 50 {
		t.Errorf("MaxFiles: got %d, want 50", cfg.Worktree.CommitPolicy.MaxFiles)
	}
	if !cfg.Worktree.CommitPolicy.RequireGitignore {
		t.Error("RequireGitignore: got false, want true")
	}

	// Round-trip
	data, err := yaml.Marshal(&cfg)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	var decoded Config
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal round-trip failed: %v", err)
	}
	if decoded.Worktree.CommitPolicy.MaxFiles != 50 {
		t.Errorf("round-trip MaxFiles: got %d, want 50", decoded.Worktree.CommitPolicy.MaxFiles)
	}
}

func TestContinuousMarshalUnmarshal(t *testing.T) {
	c := Continuous{
		SchemaVersion:    1,
		FileType:         "state_continuous",
		CurrentIteration: 0,
		MaxIterations:    10,
		Status:           ContinuousStatusStopped,
		UpdatedAt:        "2026-01-01T00:00:00Z",
	}

	data, err := yaml.Marshal(&c)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded Continuous
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded.Status != ContinuousStatusStopped {
		t.Errorf("status: got %q", decoded.Status)
	}
	if decoded.MaxIterations != 10 {
		t.Errorf("max_iterations: got %d", decoded.MaxIterations)
	}
	if decoded.UpdatedAt != "2026-01-01T00:00:00Z" {
		t.Errorf("updated_at: got %q, want %q", decoded.UpdatedAt, "2026-01-01T00:00:00Z")
	}
}

func TestConfigTemplate_ParsesSuccessfully(t *testing.T) {
	// Smoke test: verify templates/config.yaml parses into Config struct
	data, err := os.ReadFile("../../templates/config.yaml")
	if err != nil {
		t.Skipf("templates/config.yaml not found (running from non-standard location): %v", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("templates/config.yaml failed to parse: %v", err)
	}
	// Verify git_timeout_sec is loaded from template
	if cfg.Worktree.GitTimeoutSec != 120 {
		t.Errorf("template worktree.git_timeout_sec: got %d, want 120", cfg.Worktree.GitTimeoutSec)
	}
	// Verify a few other fields to ensure overall template integrity
	if cfg.Maestro.Version != "2.0.0" {
		t.Errorf("template maestro.version: got %q, want %q", cfg.Maestro.Version, "2.0.0")
	}
	if cfg.Agents.Workers.Count != 4 {
		t.Errorf("template agents.workers.count: got %d, want 4", cfg.Agents.Workers.Count)
	}
}

func TestContinuousUpdatedAt_ValueType(t *testing.T) {
	// Verify UpdatedAt is a value type (string), consistent with CommandState.UpdatedAt
	c := Continuous{
		SchemaVersion: 1,
		FileType:      "state_continuous",
		Status:        ContinuousStatusRunning,
		UpdatedAt:     "2026-03-05T12:00:00Z",
	}

	data, err := yaml.Marshal(&c)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded Continuous
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded.UpdatedAt != c.UpdatedAt {
		t.Errorf("UpdatedAt round-trip: got %q, want %q", decoded.UpdatedAt, c.UpdatedAt)
	}

	// Verify empty UpdatedAt deserializes as empty string (not nil)
	emptyYAML := []byte("schema_version: 1\nfile_type: state_continuous\nstatus: running\n")
	var empty Continuous
	if err := yaml.Unmarshal(emptyYAML, &empty); err != nil {
		t.Fatalf("Unmarshal empty failed: %v", err)
	}
	if empty.UpdatedAt != "" {
		t.Errorf("empty UpdatedAt: got %q, want empty string", empty.UpdatedAt)
	}
}
