package main

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/formation"
)

func gitInit(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.email=t@e", "-c", "user.name=t", "config", "user.email", "t@e"},
		{"-c", "user.email=t@e", "-c", "user.name=t", "config", "user.name", "t"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func TestPrecheckGitRepo_NotARepo(t *testing.T) {
	dir := t.TempDir()
	err := precheckGitRepo(dir, "main")
	if err == nil || !strings.Contains(err.Error(), "not a git repository") {
		t.Fatalf("expected not-a-repo error, got %v", err)
	}
}

func TestPrecheckGitRepo_EmptyRepo(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	err := precheckGitRepo(dir, "main")
	if err == nil || !strings.Contains(err.Error(), "no commits yet") {
		t.Fatalf("expected empty-repo error, got %v", err)
	}
}

func TestPrecheckGitRepo_MissingBaseBranch(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	gitRun(t, dir, "checkout", "-q", "-b", "develop")
	gitRun(t, dir, "commit", "-q", "--allow-empty", "-m", "init")
	err := precheckGitRepo(dir, "main")
	if err == nil || !strings.Contains(err.Error(), `does not resolve to a commit`) {
		t.Fatalf("expected missing-ref error, got %v", err)
	}
}

func TestPrecheckGitRepo_DetachedHeadSHAAccepted(t *testing.T) {
	// A detached-HEAD base (commit SHA, as maestro setup writes for a pinned
	// submodule) must pass precheck even though no branch of that name exists.
	dir := t.TempDir()
	gitInit(t, dir)
	gitRun(t, dir, "checkout", "-q", "-b", "main")
	gitRun(t, dir, "commit", "-q", "--allow-empty", "-m", "init")
	sha := gitOutput(t, dir, "rev-parse", "HEAD")
	gitRun(t, dir, "checkout", "-q", "--detach", sha)
	if err := precheckGitRepo(dir, sha); err != nil {
		t.Fatalf("expected detached-HEAD SHA base to pass precheck, got %v", err)
	}
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestPrecheckGitRepo_OK(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	gitRun(t, dir, "checkout", "-q", "-b", "main")
	gitRun(t, dir, "commit", "-q", "--allow-empty", "-m", "init")
	if err := precheckGitRepo(dir, "main"); err != nil {
		t.Fatalf("expected OK, got %v", err)
	}
}

func TestPrecheckGitRepo_Subdir(t *testing.T) {
	// projectRoot may be a subdirectory of the worktree; git rev-parse should still succeed.
	dir := t.TempDir()
	gitInit(t, dir)
	gitRun(t, dir, "checkout", "-q", "-b", "main")
	gitRun(t, dir, "commit", "-q", "--allow-empty", "-m", "init")
	sub := filepath.Join(dir, "sub")
	if err := exec.Command("mkdir", sub).Run(); err != nil {
		t.Fatal(err)
	}
	if err := precheckGitRepo(sub, "main"); err != nil {
		t.Fatalf("expected OK in subdir, got %v", err)
	}
}

func TestRunDown_FlagParsing(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"unknown flag", []string{"--unknown"}},
		{"unexpected arg", []string{"extra"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runDown(tt.args)
			if err == nil {
				t.Fatal("expected error")
			}
			var ce *CLIError
			if !errors.As(err, &ce) {
				t.Fatalf("expected CLIError, got %T: %v", err, err)
			}
		})
	}
}

func TestValidateBranchName(t *testing.T) {
	valid := []string{"main", "develop", "feature/foo", "release-1.0", "my-branch"}
	for _, name := range valid {
		if err := validateBranchName(name); err != nil {
			t.Errorf("validateBranchName(%q) = %v, want nil", name, err)
		}
	}

	invalid := []struct {
		name    string
		wantMsg string
	}{
		{"", "empty"},
		{"foo..bar", ".."},
		{"foo~bar", "~"},
		{"foo^bar", "^"},
		{"foo:bar", ":"},
		{"foo\\bar", "\\"},
		{"foo bar", " "},
		{"foo?bar", "?"},
		{"foo*bar", "*"},
		{"-start", "hyphen"},
		{"foo.lock", ".lock"},
		{"foo.", "dot"},
		{"/foo", "slash"},
		{"foo/", "slash"},
		{"foo//bar", "slash"},
		{"foo\x01bar", "control"},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			err := validateBranchName(tt.name)
			if err == nil {
				t.Errorf("validateBranchName(%q) = nil, want error containing %q", tt.name, tt.wantMsg)
			}
		})
	}
}

func TestPrecheckGitRepo_InvalidBranchName(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	gitRun(t, dir, "checkout", "-q", "-b", "main")
	gitRun(t, dir, "commit", "-q", "--allow-empty", "-m", "init")

	err := precheckGitRepo(dir, "foo..bar")
	if err == nil || !strings.Contains(err.Error(), "invalid base branch name") {
		t.Fatalf("expected invalid branch name error, got %v", err)
	}
}

func TestRunUp_FlagParsing(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{"unknown flag", []string{"--unknown"}, true},
		{"unexpected arg", []string{"extra"}, true},
		{"double dash unknown", []string{"--boost", "--unknown"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runUp(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				var ce *CLIError
				if !errors.As(err, &ce) {
					t.Fatalf("expected CLIError, got %T: %v", err, err)
				}
			}
		})
	}
}

func TestSkipRunUpCleanup(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"session exists", fmt.Errorf("maestro up: %w", formation.ErrSessionExists), true},
		{"preflight failed", fmt.Errorf("maestro up: %w", formation.ErrPreflightFailed), true},
		{"sandboxed launch", fmt.Errorf("maestro up: %w", formation.ErrSandboxedLaunch), true},
		{"partial startup failure", fmt.Errorf("create formation: boom"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := skipRunUpCleanup(tt.err); got != tt.want {
				t.Fatalf("skipRunUpCleanup(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
