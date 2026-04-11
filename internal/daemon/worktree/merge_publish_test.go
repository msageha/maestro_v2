package worktree

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBuildMergeMessage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		workerID       string
		workerPurposes map[string]string
		want           string
	}{
		{
			name:           "with purpose",
			workerID:       "worker1",
			workerPurposes: map[string]string{"worker1": "add login API"},
			want:           "merge: add login API",
		},
		{
			name:           "nil purposes map",
			workerID:       "worker1",
			workerPurposes: nil,
			want:           "merge: worker1 changes",
		},
		{
			name:           "worker not in map",
			workerID:       "worker2",
			workerPurposes: map[string]string{"worker1": "some purpose"},
			want:           "merge: worker2 changes",
		},
		{
			name:           "empty purpose",
			workerID:       "worker1",
			workerPurposes: map[string]string{"worker1": ""},
			want:           "merge: worker1 changes",
		},
		{
			name:           "long purpose truncated to 72 chars",
			workerID:       "worker1",
			workerPurposes: map[string]string{"worker1": strings.Repeat("a", 100)},
			want:           "merge: " + strings.Repeat("a", 65),
		},
		{
			name:           "multiline purpose uses first line only",
			workerID:       "worker1",
			workerPurposes: map[string]string{"worker1": "first line\nsecond line"},
			want:           "merge: first line",
		},
		{
			name:           "no maestro prefix",
			workerID:       "worker1",
			workerPurposes: map[string]string{"worker1": "add feature"},
			want:           "merge: add feature",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := buildMergeMessage(tt.workerID, tt.workerPurposes)
			if got != tt.want {
				t.Errorf("buildMergeMessage() = %q, want %q", got, tt.want)
			}
			if len(got) > mergePublishMaxLen {
				t.Errorf("buildMergeMessage() length %d exceeds max %d", len(got), mergePublishMaxLen)
			}
			if strings.Contains(got, "[maestro]") {
				t.Errorf("buildMergeMessage() should not contain [maestro] prefix")
			}
		})
	}
}

func TestBuildPublishMessage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		publishMessage string
		baseBranch     string
		want           string
	}{
		{
			name:           "with content",
			publishMessage: "ユーザー認証機能を実装する",
			baseBranch:     "main",
			want:           "publish: ユーザー認証機能を実装する",
		},
		{
			name:           "empty content fallback",
			publishMessage: "",
			baseBranch:     "main",
			want:           "publish: integrate changes to main",
		},
		{
			name:           "long content truncated",
			publishMessage: strings.Repeat("b", 100),
			baseBranch:     "main",
			want:           "publish: " + strings.Repeat("b", 63),
		},
		{
			name:           "multiline content uses first line",
			publishMessage: "first line\nsecond line\nthird line",
			baseBranch:     "main",
			want:           "publish: first line",
		},
		{
			name:           "no maestro prefix",
			publishMessage: "deploy pipeline",
			baseBranch:     "main",
			want:           "publish: deploy pipeline",
		},
		{
			name:           "different base branch in fallback",
			publishMessage: "",
			baseBranch:     "develop",
			want:           "publish: integrate changes to develop",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := buildPublishMessage(tt.publishMessage, tt.baseBranch)
			if got != tt.want {
				t.Errorf("buildPublishMessage() = %q, want %q", got, tt.want)
			}
			if len(got) > mergePublishMaxLen {
				t.Errorf("buildPublishMessage() length %d exceeds max %d", len(got), mergePublishMaxLen)
			}
			if strings.Contains(got, "[maestro]") {
				t.Errorf("buildPublishMessage() should not contain [maestro] prefix")
			}
		})
	}
}

func TestTruncateMessage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		prefix string
		body   string
		maxLen int
		want   string
	}{
		{
			name:   "short message unchanged",
			prefix: "merge: ",
			body:   "hello",
			maxLen: 72,
			want:   "merge: hello",
		},
		{
			name:   "truncated at maxLen",
			prefix: "merge: ",
			body:   strings.Repeat("x", 100),
			maxLen: 20,
			want:   "merge: xxxxxxxxxxxxx",
		},
		{
			name:   "empty body returns prefix only",
			prefix: "merge: ",
			body:   "",
			maxLen: 72,
			want:   "merge: ",
		},
		{
			name:   "whitespace-only body returns prefix",
			prefix: "publish: ",
			body:   "   \t  ",
			maxLen: 72,
			want:   "publish: ",
		},
		{
			name:   "newline takes first line",
			prefix: "p: ",
			body:   "line1\nline2",
			maxLen: 72,
			want:   "p: line1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := truncateMessage(tt.prefix, tt.body, tt.maxLen)
			if got != tt.want {
				t.Errorf("truncateMessage() = %q, want %q", got, tt.want)
			}
			if tt.maxLen > 0 && len(got) > tt.maxLen {
				t.Errorf("truncateMessage() length %d exceeds max %d", len(got), tt.maxLen)
			}
		})
	}
}

// TestMergeToIntegration_PathGuardRejectsEscape verifies that MergeToIntegration
// refuses to operate when the integration worktree path escapes the project root
// (e.g. via symlink). This is defense-in-depth for the recovery paths that use
// git reset --hard + clean -fd.
func TestMergeToIntegration_PathGuardRejectsEscape(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	commandID := "cmd_pathguard"
	workers := []string{"worker1"}
	if err := createForCommand(wm, commandID, workers); err != nil {
		t.Fatalf("CreateForCommand: %v", err)
	}

	// Replace the integration worktree with a symlink escaping projectRoot.
	integrationPath := filepath.Join(projectRoot, ".maestro", "worktrees", commandID, "_integration")

	// Remove the real git worktree first (gitRun does not take the mutex).
	_ = wm.gitRun("worktree", "remove", "--force", integrationPath)
	_ = os.RemoveAll(integrationPath)

	// Create an outside directory and symlink to it.
	outside := t.TempDir()
	if err := os.Symlink(outside, integrationPath); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	// MergeToIntegration should refuse due to pathGuard.
	_, err := wm.MergeToIntegration(commandID, workers, nil)
	if err == nil {
		t.Fatal("expected path guard error, got nil")
	}
	if !strings.Contains(err.Error(), "merge to integration refused") {
		t.Errorf("expected path guard error message, got: %v", err)
	}
}
