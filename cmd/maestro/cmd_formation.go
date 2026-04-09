package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/msageha/maestro_v2/internal/formation"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/tmux"
	"github.com/msageha/maestro_v2/internal/validate"
)

// precheckGitRepo verifies that projectRoot is inside a git repository and that
// baseBranch exists with at least one commit. The dispatcher creates worktrees
// from baseBranch via `git worktree add`, which fails hard if either condition
// is not met — surface a clear error here before any tmux/daemon resources are
// spun up.
func precheckGitRepo(projectRoot, baseBranch string) error {
	// 1. Inside a git work tree?
	cmd := exec.Command("git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = projectRoot
	out, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		return fmt.Errorf("not a git repository: %s\n  hint: run `git init` and create at least one commit on %q before `maestro up`", projectRoot, baseBranch)
	}

	// 2. Base branch exists and points to a real commit?
	cmd = exec.Command("git", "rev-parse", "--verify", "--quiet", "refs/heads/"+baseBranch)
	cmd.Dir = projectRoot
	if err := cmd.Run(); err != nil {
		// Distinguish "no commits yet" (HEAD unborn) from "branch missing".
		head := exec.Command("git", "rev-parse", "--verify", "--quiet", "HEAD")
		head.Dir = projectRoot
		if headErr := head.Run(); headErr != nil {
			return fmt.Errorf("base branch %q has no commits yet (repository is empty)\n  hint: create an initial commit, e.g. `git commit --allow-empty -m init`", baseBranch)
		}
		return fmt.Errorf("base branch %q does not exist in %s\n  hint: create it (e.g. `git branch %s`) or set worktree.base_branch in .maestro/config.yaml to an existing branch", baseBranch, projectRoot, baseBranch)
	}
	return nil
}

// runUp starts the formation (daemon + agents) and optionally attaches to tmux.
func runUp(args []string) error {
	fs := newFlagSet("maestro up")
	var boost, continuous, detach, force bool
	fs.BoolVar(&boost, "boost", false, "")
	fs.BoolVar(&continuous, "continuous", false, "")
	fs.BoolVar(&detach, "detach", false, "")
	fs.BoolVar(&detach, "d", false, "")
	fs.BoolVar(&force, "force", false, "")
	fs.BoolVar(&force, "f", false, "")
	if err := fs.Parse(args); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro up: %v\nusage: maestro up [--boost] [--continuous] [--detach|-d] [--force|-f]", err)}
	}
	if fs.NArg() > 0 {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro up: unexpected argument: %s\nusage: maestro up [--boost] [--continuous] [--detach|-d] [--force|-f]", fs.Arg(0))}
	}

	maestroDir, err := requireMaestroDir("up")
	if err != nil {
		return err
	}

	cfg, err := model.LoadConfig(maestroDir)
	if err != nil {
		return fmt.Errorf("maestro up: load config: %w", err)
	}

	// Validate project name before use in tmux session name
	if err := validate.ValidateProjectName(cfg.Project.Name); err != nil {
		return fmt.Errorf("maestro up: invalid project name: %w", err)
	}
	tmux.SetSessionName("maestro-" + cfg.Project.Name)

	// Precheck: dispatcher creates per-worker worktrees from the base branch via
	// `git worktree add`. If the project is not a git repo, or the base branch
	// has no commits, every task fails before dispatch. Catch this up-front so
	// users get a clear, actionable message instead of opaque dispatch errors.
	projectRoot := filepath.Dir(maestroDir)
	baseBranch := cfg.Worktree.EffectiveBaseBranch()
	if err := precheckGitRepo(projectRoot, baseBranch); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro up: %v", err)}
	}

	opts := formation.UpOptions{
		MaestroDir:    maestroDir,
		Config:        cfg,
		Boost:         boost,
		Continuous:    continuous,
		Force:         force,
		BoostSet:      boost,
		ContinuousSet: continuous,
	}

	if err := formation.RunUp(opts); err != nil {
		// Clean up any partially-created resources (tmux session, daemon)
		fmt.Fprintln(os.Stderr, "Cleaning up after setup failure...")
		if cleanupErr := formation.CleanupOnFailure(maestroDir); cleanupErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: cleanup encountered errors: %v\n", cleanupErr)
			return errors.Join(fmt.Errorf("maestro up: %w", err), fmt.Errorf("cleanup: %w", cleanupErr))
		}
		return fmt.Errorf("maestro up: %w", err)
	}

	if !detach {
		if os.Getenv("TMUX") != "" {
			fmt.Printf("Already inside tmux. Attach with: tmux switch-client -t %s\n", tmux.GetSessionName())
			return nil
		}
		if err := tmux.AttachSession(); err != nil {
			return fmt.Errorf("maestro up: attach: %w", err)
		}
	}
	return nil
}

// runDown gracefully shuts down the formation.
func runDown(args []string) error {
	fs := newFlagSet("maestro down")
	if err := fs.Parse(args); err != nil {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro down: %v\nusage: maestro down", err)}
	}
	if fs.NArg() > 0 {
		return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro down: unexpected argument: %s\nusage: maestro down", fs.Arg(0))}
	}

	maestroDir, err := requireMaestroDir("down")
	if err != nil {
		return err
	}

	cfg, err := model.LoadConfig(maestroDir)
	if err != nil {
		// Config may be corrupt, but 'down' must still be able to stop the daemon.
		// Proceed with zero config — UDS/PID-based shutdown works without it.
		fmt.Fprintf(os.Stderr, "Warning: could not load config: %v\nProceeding with default config.\n", err)
	}

	if err := formation.RunDown(maestroDir, cfg); err != nil {
		return fmt.Errorf("maestro down: %w", err)
	}
	return nil
}
