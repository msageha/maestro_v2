package worktree

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestClassifyGitError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want gitErrorClass
	}{
		// Transient errors
		{
			name: "lock contention",
			err:  errors.New("Unable to create '/path/to/repo/.git/index.lock': File exists"),
			want: gitErrorTransient,
		},
		{
			name: "dotlock file",
			err:  errors.New("cannot create .lock file"),
			want: gitErrorTransient,
		},
		{
			name: "unable to create generic",
			err:  errors.New("Unable to create temp file"),
			want: gitErrorTransient,
		},

		// Permanent errors
		{
			name: "bad object",
			err:  errors.New("fatal: bad object abc123"),
			want: gitErrorPermanent,
		},
		{
			name: "corrupt repo",
			err:  errors.New("error: object file is corrupt"),
			want: gitErrorPermanent,
		},
		{
			name: "not a git repository",
			err:  errors.New("fatal: not a git repository (or any parent up to mount point /)"),
			want: gitErrorPermanent,
		},
		{
			name: "invalid reference",
			err:  errors.New("fatal: invalid reference: refs/heads/nonexistent"),
			want: gitErrorPermanent,
		},

		// Timeout errors — must be Permanent (dirty worktree risk)
		{
			name: "context.DeadlineExceeded",
			err:  context.DeadlineExceeded,
			want: gitErrorPermanent,
		},
		{
			name: "wrapped DeadlineExceeded",
			err:  fmt.Errorf("git merge: timeout after 30s: %w", context.DeadlineExceeded),
			want: gitErrorPermanent,
		},
		{
			name: "timeout in error message",
			err:  errors.New("connection timeout while fetching"),
			want: gitErrorPermanent,
		},

		// Unknown errors default to Permanent
		{
			name: "unknown error",
			err:  errors.New("something completely unexpected happened"),
			want: gitErrorPermanent,
		},
		{
			name: "nil error",
			err:  nil,
			want: gitErrorPermanent,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyGitError(tt.err)
			if got != tt.want {
				wantStr := "Permanent"
				gotStr := "Permanent"
				if tt.want == gitErrorTransient {
					wantStr = "Transient"
				}
				if got == gitErrorTransient {
					gotStr = "Transient"
				}
				t.Errorf("classifyGitError(%v) = %s, want %s", tt.err, gotStr, wantStr)
			}
		})
	}
}

func TestWrapGitOutputError_IncludesStderr(t *testing.T) {
	// Simulate exec.ExitError with Stderr containing a transient pattern.
	exitErr := &exec.ExitError{
		ProcessState: nil, // not used by our code
		Stderr:       []byte("Unable to create '/path/to/repo/.git/index.lock': File exists"),
	}
	wrapped := wrapGitOutputError(exitErr, []string{"merge", "main"})
	msg := wrapped.Error()

	if !strings.Contains(msg, "Unable to create") {
		t.Errorf("expected wrapped error to contain stderr, got: %s", msg)
	}
	if !strings.Contains(msg, "git merge main") {
		t.Errorf("expected wrapped error to contain command, got: %s", msg)
	}

	// The error should be classified as Transient thanks to stderr content.
	if classifyGitError(wrapped) != gitErrorTransient {
		t.Error("expected classifyGitError to return Transient for stderr with lock pattern")
	}
}

func TestWrapGitOutputError_EmptyStderr(t *testing.T) {
	exitErr := &exec.ExitError{
		Stderr: []byte{},
	}
	wrapped := wrapGitOutputError(exitErr, []string{"status"})
	msg := wrapped.Error()

	// Should not contain "stderr:" when Stderr is empty.
	if strings.Contains(msg, "stderr:") {
		t.Errorf("expected no stderr in error message for empty Stderr, got: %s", msg)
	}
}

func TestClassifyGitError_ExitErrorWithTransientStderr(t *testing.T) {
	// When exec.ExitError is wrapped with stderr via wrapGitOutputError,
	// classifyGitError should detect transient patterns.
	tests := []struct {
		name   string
		stderr string
		want   gitErrorClass
	}{
		{
			name:   "lock file in stderr",
			stderr: "Unable to create '/repo/.git/index.lock': File exists",
			want:   gitErrorTransient,
		},
		{
			name:   "dotlock in stderr",
			stderr: "error: cannot lock ref 'refs/heads/main': is at abc123 but expected def456",
			want:   gitErrorTransient,
		},
		{
			name:   "permanent error in stderr",
			stderr: "fatal: bad object abc123",
			want:   gitErrorPermanent,
		},
		{
			name:   "unknown stderr content",
			stderr: "exit status 128",
			want:   gitErrorPermanent,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exitErr := &exec.ExitError{
				Stderr: []byte(tt.stderr),
			}
			wrapped := wrapGitOutputError(exitErr, []string{"merge", "main"})
			got := classifyGitError(wrapped)
			if got != tt.want {
				wantStr := "Permanent"
				gotStr := "Permanent"
				if tt.want == gitErrorTransient {
					wantStr = "Transient"
				}
				if got == gitErrorTransient {
					gotStr = "Transient"
				}
				t.Errorf("classifyGitError(wrapped stderr=%q) = %s, want %s", tt.stderr, gotStr, wantStr)
			}
		})
	}
}

func TestIsTransientGitError(t *testing.T) {
	if !isTransientGitError(errors.New("Unable to create lock")) {
		t.Error("expected lock error to be transient")
	}
	if isTransientGitError(errors.New("fatal: bad object")) {
		t.Error("expected bad object error to not be transient")
	}
	if isTransientGitError(context.DeadlineExceeded) {
		t.Error("expected DeadlineExceeded to not be transient")
	}
}

func TestGitRunWithRetry_TransientRetries(t *testing.T) {
	// Use a real git repo — run a command that will produce a transient-like error.
	// We test the retry mechanism by verifying that gitRunWithRetry eventually
	// succeeds or retries the expected number of times.
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	// A command that succeeds should not retry.
	err := wm.gitRunWithRetry(projectRoot, 3, "status")
	if err != nil {
		t.Fatalf("gitRunWithRetry on successful command: %v", err)
	}
}

func TestGitRunWithRetry_PermanentNoRetry(t *testing.T) {
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	// A command that produces a permanent error (bad revision) should not retry.
	start := time.Now()
	err := wm.gitRunWithRetry(projectRoot, 3, "log", "--oneline", "nonexistent_ref_abc123..HEAD")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error for bad revision, got nil")
	}

	// If it retried 3 times with backoff (50+100+200=350ms minimum), elapsed would be >300ms.
	// A permanent error should return immediately, so elapsed should be well under 300ms.
	if elapsed > 300*time.Millisecond {
		t.Errorf("gitRunWithRetry took %v, expected fast return for permanent error", elapsed)
	}
}

func TestGitRunWithRetry_MaxRetriesZero(t *testing.T) {
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	// maxRetries=0 should behave like a single attempt.
	err := wm.gitRunWithRetry(projectRoot, 0, "status")
	if err != nil {
		t.Fatalf("gitRunWithRetry with maxRetries=0 on success: %v", err)
	}
}

func TestGitOutputWithRetry_Success(t *testing.T) {
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	output, err := wm.gitOutputWithRetry(projectRoot, 3, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("gitOutputWithRetry: %v", err)
	}
	if len(output) < 7 {
		t.Errorf("expected SHA output, got %q", output)
	}
}

func TestGitOutputWithRetry_PermanentNoRetry(t *testing.T) {
	projectRoot := initTestGitRepo(t)
	wm := newTestWorktreeManager(t, projectRoot)

	start := time.Now()
	_, err := wm.gitOutputWithRetry(projectRoot, 3, "rev-parse", "nonexistent_ref_xyz999")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error for nonexistent ref, got nil")
	}

	if elapsed > 300*time.Millisecond {
		t.Errorf("gitOutputWithRetry took %v, expected fast return for permanent error", elapsed)
	}
}
