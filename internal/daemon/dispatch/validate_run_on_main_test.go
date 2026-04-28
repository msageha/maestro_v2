package dispatch

import (
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

// stubResolver is a test double satisfying WorktreeResolver. Only
// GetIntegrationStatus is exercised by the publish-guard tests; the other
// methods return zero values they do not need.
type stubResolver struct {
	status model.IntegrationStatus
	err    error
}

func (s stubResolver) GetWorkerPath(string, string) (string, error)   { return "", nil }
func (s stubResolver) EnsureWorkerWorktree(string, string) error      { return nil }
func (s stubResolver) GetIntegrationPath(string) (string, error)      { return "", nil }
func (s stubResolver) EnsureIntegrationBranchCheckedOut(string) error { return nil }
func (s stubResolver) GetIntegrationStatus(string) (model.IntegrationStatus, error) {
	return s.status, s.err
}

func TestValidateRunOnMainContent_AllowsBenignTasks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		task *model.Task
	}{
		{
			name: "nil task",
			task: nil,
		},
		{
			name: "neither run_on_main nor run_on_integration set",
			task: &model.Task{Content: "git push origin main"},
		},
		{
			name: "run_on_main with safe content",
			task: &model.Task{RunOnMain: true, Content: "go test ./..."},
		},
		{
			name: "run_on_integration with safe content",
			task: &model.Task{RunOnIntegration: true, Content: "git status; go vet ./..."},
		},
		{
			name: "prose mentioning git push without command",
			task: &model.Task{RunOnMain: true, Content: "Confirm that the team did not push old commits."},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := validateRunOnMainContent(tt.task); err != nil {
				t.Fatalf("expected nil error, got: %v", err)
			}
		})
	}
}

func TestValidateRunOnMainContent_RejectsDestructivePatterns(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
	}{
		{name: "git push origin main", content: "git push origin main"},
		{name: "git push --force", content: "git push --force origin main"},
		{name: "git push -f", content: "git push -f"},
		{name: "git reset --hard", content: "git reset --hard HEAD~1"},
		{name: "git checkout --", content: "git checkout -- main.go"},
		{name: "git branch -D", content: "git branch -D feature/foo"},
		{name: "git clean -fd", content: "git clean -fd"},
		{name: "git update-ref -d", content: "git update-ref -d refs/heads/foo"},
		{name: "rm -rf", content: "rm -rf node_modules"},
		{name: "rm -fr", content: "rm -fr build"},
		{name: "case insensitive Git Push", content: "Git Push --force origin main"},
		{name: "embedded in script", content: "go build && git push origin main"},
	}
	for _, tt := range tests {
		t.Run("RunOnMain_"+tt.name, func(t *testing.T) {
			t.Parallel()
			task := &model.Task{RunOnMain: true, Content: tt.content}
			err := validateRunOnMainContent(task)
			if err == nil {
				t.Fatalf("expected destructive content to be rejected, got nil")
			}
			if !errors.Is(err, ErrDestructiveContentRejected) {
				t.Fatalf("expected ErrDestructiveContentRejected, got: %v", err)
			}
		})
		t.Run("RunOnIntegration_"+tt.name, func(t *testing.T) {
			t.Parallel()
			task := &model.Task{RunOnIntegration: true, Content: tt.content}
			err := validateRunOnMainContent(task)
			if err == nil {
				t.Fatalf("expected destructive content to be rejected, got nil")
			}
			if !errors.Is(err, ErrDestructiveContentRejected) {
				t.Fatalf("expected ErrDestructiveContentRejected, got: %v", err)
			}
		})
	}
}

func TestValidateRunOnMainContent_RejectsDestructivePatternsOutsideContent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		task *model.Task
	}{
		{
			name: "acceptance criteria",
			task: &model.Task{RunOnMain: true, Content: "go test ./...", AcceptanceCriteria: "then run git reset --hard HEAD"},
		},
		{
			name: "constraints",
			task: &model.Task{RunOnIntegration: true, Content: "resolve conflict", Constraints: []string{"finish with git push origin main"}},
		},
		{
			name: "persona hint",
			task: &model.Task{RunOnMain: true, Content: "inspect", PersonaHint: "use rm -rf build to clean"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateRunOnMainContent(tt.task)
			if err == nil {
				t.Fatalf("expected destructive instruction to be rejected")
			}
			if !errors.Is(err, ErrDestructiveContentRejected) {
				t.Fatalf("expected ErrDestructiveContentRejected, got: %v", err)
			}
		})
	}
}

func TestValidateRunOnMainContent_AllowsNegationDirectives(t *testing.T) {
	t.Parallel()
	// The Planner frequently writes prohibitions of destructive ops in
	// constraints/persona_hint of read-only RunOnMain tasks. A naive regex
	// would falsely reject them. The validator must skip matches whose
	// enclosing line is a negation directive.
	tests := []struct {
		name string
		task *model.Task
	}{
		{
			name: "japanese constraint prohibiting git push",
			task: &model.Task{
				RunOnMain: true,
				Content:   "diagnose drift on main",
				Constraints: []string{
					"git push 等の変更系 git 操作を行わないこと",
				},
			},
		},
		{
			name: "english constraint prohibiting rm -rf",
			task: &model.Task{
				RunOnIntegration: true,
				Content:          "inspect integration worktree",
				Constraints:      []string{"do not run rm -rf or git reset --hard"},
			},
		},
		{
			name: "persona hint with avoid",
			task: &model.Task{
				RunOnMain:   true,
				Content:     "audit",
				PersonaHint: "avoid git push at all costs",
			},
		},
		{
			name: "japanese forbidden marker",
			task: &model.Task{
				RunOnMain:          true,
				Content:            "verify",
				AcceptanceCriteria: "git push origin main の実行は禁止",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := validateRunOnMainContent(tt.task); err != nil {
				t.Fatalf("expected negation directive to be allowed, got: %v", err)
			}
		})
	}
}

func TestValidateRunOnMainContent_FlagsCommandAfterNegationOnSameLine(t *testing.T) {
	t.Parallel()
	// Regression: previously the negation check examined the whole line, so a
	// single "Avoid" / "do not" anywhere on the line suppressed every
	// destructive pattern on that line. Codex / Gemini workers do not honour
	// the Bash policy hook, so the pre-flight is the only safety net — any
	// destructive command that appears AFTER a clause separator must be
	// rejected even when an unrelated negation precedes it.
	tests := []struct {
		name    string
		content string
	}{
		{
			name:    "english semicolon-separated clauses",
			content: "Avoid breaking tests; run git push origin main",
		},
		{
			name:    "japanese full-stop separated clauses",
			content: "破壊的操作はしない。続けて git push origin main を実行",
		},
		{
			name:    "english full-width semicolon",
			content: "do not refactor；then git push origin main",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			task := &model.Task{RunOnMain: true, Content: tt.content}
			err := validateRunOnMainContent(task)
			if err == nil {
				t.Fatalf("expected destructive command after clause boundary to be rejected, got nil for content %q", tt.content)
			}
			if !errors.Is(err, ErrDestructiveContentRejected) {
				t.Fatalf("expected ErrDestructiveContentRejected, got: %v", err)
			}
		})
	}
}

func TestValidateRunOnMainContent_AllowsNegationInSameClause(t *testing.T) {
	t.Parallel()
	// Companion to the regression test above: when negation and the
	// destructive pattern share a clause (no boundary between them), the
	// validator must still suppress the rejection. This guards against the
	// fix over-correcting.
	tests := []struct {
		name string
		task *model.Task
	}{
		{
			name: "japanese negation after pattern, no boundary",
			task: &model.Task{
				RunOnMain: true,
				Content:   "git push origin main の実行は禁止",
			},
		},
		{
			name: "english negation before pattern with colon (not a boundary)",
			task: &model.Task{
				RunOnIntegration: true,
				Content:          "Forbidden commands include: git push origin main and git reset --hard",
			},
		},
		{
			name: "japanese negation before pattern via te-form",
			task: &model.Task{
				RunOnMain: true,
				Content:   "git push を行わずに git status のみで状態を確認",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := validateRunOnMainContent(tt.task); err != nil {
				t.Fatalf("expected negation in same clause to suppress rejection, got: %v", err)
			}
		})
	}
}

func TestValidateRunOnMainContent_FlagsExecutableLineEvenWithNegationElsewhere(t *testing.T) {
	t.Parallel()
	// Negation suppression is per-line, so a separate line that *executes*
	// a destructive command must still be rejected even when another line in
	// the same field carries a prohibition. This guards against a Planner
	// accidentally mixing prohibitions and live commands in the same field.
	task := &model.Task{
		RunOnMain: true,
		Content:   "step 1: git status\nstep 2: git push origin main\nnote: do not push if dirty",
	}
	err := validateRunOnMainContent(task)
	if err == nil {
		t.Fatalf("expected destructive content on a non-negated line to be rejected")
	}
	if !errors.Is(err, ErrDestructiveContentRejected) {
		t.Fatalf("expected ErrDestructiveContentRejected, got: %v", err)
	}
}

func TestValidateRunOnMainContent_WorktreeTasksUnaffected(t *testing.T) {
	t.Parallel()
	// Destructive content inside a worktree-isolated task is recoverable
	// (worker worktrees are disposable), so the check intentionally skips it.
	task := &model.Task{
		RunOnMain:        false,
		RunOnIntegration: false,
		Content:          "git push --force origin worker-branch",
	}
	if err := validateRunOnMainContent(task); err != nil {
		t.Fatalf("worktree-isolated task should not be rejected, got: %v", err)
	}
}

func TestValidateRunOnMainPublishGuard(t *testing.T) {
	t.Parallel()

	makeRunOnMain := func() *model.Task {
		return &model.Task{
			ID:        "task_run_on_main",
			CommandID: "cmd_publish_guard",
			RunOnMain: true,
			Content:   "go test ./...",
		}
	}

	tests := []struct {
		name    string
		task    *model.Task
		res     WorktreeResolver
		wantErr error
	}{
		{
			name:    "nil task is no-op",
			task:    nil,
			res:     stubResolver{status: model.IntegrationStatusMerged},
			wantErr: nil,
		},
		{
			name:    "non run_on_main task skips guard",
			task:    &model.Task{ID: "t1", CommandID: "c1", Content: "go vet"},
			res:     stubResolver{status: model.IntegrationStatusMerged},
			wantErr: nil,
		},
		{
			name:    "nil resolver (worktree disabled) skips guard",
			task:    makeRunOnMain(),
			res:     nil,
			wantErr: nil,
		},
		{
			name:    "missing worktree state file skips guard",
			task:    makeRunOnMain(),
			res:     stubResolver{err: fmt.Errorf("read state: %w", os.ErrNotExist)},
			wantErr: nil,
		},
		{
			name:    "integration published allows dispatch",
			task:    makeRunOnMain(),
			res:     stubResolver{status: model.IntegrationStatusPublished},
			wantErr: nil,
		},
		{
			name:    "integration merged but not published is rejected",
			task:    makeRunOnMain(),
			res:     stubResolver{status: model.IntegrationStatusMerged},
			wantErr: ErrRunOnMainBeforePublish,
		},
		{
			name:    "integration created (initial) is rejected",
			task:    makeRunOnMain(),
			res:     stubResolver{status: model.IntegrationStatusCreated},
			wantErr: ErrRunOnMainBeforePublish,
		},
		{
			name:    "integration quarantined is rejected",
			task:    makeRunOnMain(),
			res:     stubResolver{status: model.IntegrationStatusQuarantined},
			wantErr: ErrRunOnMainBeforePublish,
		},
		{
			name:    "non-ErrNotExist resolver error propagates",
			task:    makeRunOnMain(),
			res:     stubResolver{err: errors.New("disk failure")},
			wantErr: nil, // assertion below uses errors.Is on os.ErrNotExist; non-nil is enough
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateRunOnMainPublishGuard(tt.task, tt.res)
			switch {
			case tt.wantErr == nil && tt.name != "non-ErrNotExist resolver error propagates":
				if err != nil {
					t.Fatalf("expected nil error, got: %v", err)
				}
			case tt.name == "non-ErrNotExist resolver error propagates":
				if err == nil {
					t.Fatalf("expected propagated error, got nil")
				}
				if errors.Is(err, ErrRunOnMainBeforePublish) {
					t.Fatalf("disk error should not be classified as before-publish: %v", err)
				}
			default:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("expected %v, got: %v", tt.wantErr, err)
				}
			}
		})
	}
}
