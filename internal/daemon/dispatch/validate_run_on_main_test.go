package dispatch

import (
	"errors"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

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
