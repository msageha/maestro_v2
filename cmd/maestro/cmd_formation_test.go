package main

import (
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
	if err == nil || !strings.Contains(err.Error(), `does not exist`) {
		t.Fatalf("expected missing-branch error, got %v", err)
	}
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
