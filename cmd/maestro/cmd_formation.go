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
)

// precheckGitRepo verifies that projectRoot is inside a git repository and that
// baseBranch exists with at least one commit. The dispatcher creates worktrees
// from baseBranch via `git worktree add`, which fails hard if either condition
// is not met — surface a clear error here before any tmux/daemon resources are
// spun up.
func precheckGitRepo(projectRoot, baseBranch string) error {
	// 0. Validate branch name
	if err := validateBranchName(baseBranch); err != nil {
		return fmt.Errorf("invalid base branch name: %w", err)
	}

	// 1. Inside a git work tree?
	cmd := exec.Command("git", "rev-parse", "--is-inside-work-tree")
	cmd.Dir = projectRoot
	out, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		return fmt.Errorf("not a git repository: %s\n  hint: run `git init` and create at least one commit on %q before `maestro up`", projectRoot, baseBranch)
	}

	// 2. Base ref resolves to a real commit? Accept either a branch or any
	//    committish (commit SHA / tag), so a detached-HEAD base — which
	//    `maestro setup` writes as the commit SHA when the project root is on a
	//    detached HEAD, e.g. a git submodule pinned to a commit — is valid.
	//    `<ref>^{commit}` resolves a branch, SHA, or tag to a commit and fails
	//    only when nothing matches. baseBranch is validated above; the appended
	//    `^{commit}` peel suffix is a code literal, not passed through a shell.
	cmd = exec.Command("git", "rev-parse", "--verify", "--quiet", baseBranch+"^{commit}") //nolint:gosec // baseBranch is validated before use
	cmd.Dir = projectRoot
	if err := cmd.Run(); err != nil {
		// Distinguish "no commits yet" (HEAD unborn) from "ref missing".
		head := exec.Command("git", "rev-parse", "--verify", "--quiet", "HEAD")
		head.Dir = projectRoot
		if headErr := head.Run(); headErr != nil {
			return fmt.Errorf("base ref %q has no commits yet (repository is empty)\n  hint: create an initial commit, e.g. `git commit --allow-empty -m init`", baseBranch)
		}
		return fmt.Errorf("base ref %q does not resolve to a commit in %s\n  hint: set worktree.base_branch in .maestro/config.yaml to an existing branch or commit SHA (maestro setup fills this from the current HEAD automatically)", baseBranch, projectRoot)
	}
	return nil
}

// upFlagState captures the parsed `maestro up` flags. boostSet /
// continuousSet record whether the flag appeared on the command line at
// all (flag.Visit semantics), so an explicit `--boost=false` /
// `--continuous=false` is propagated as an override instead of being
// silently dropped as "flag not given".
type upFlagState struct {
	boost, continuous, detach, force bool
	boostSet, continuousSet          bool
}

// parseUpFlags parses `maestro up` arguments into an upFlagState.
func parseUpFlags(args []string) (*upFlagState, error) {
	cmd := NewCommand("maestro up", "maestro up [--boost[=bool]] [--continuous[=bool]] [--detach|-d] [--force|-f]")
	st := &upFlagState{}
	cmd.BoolVar(&st.boost, "boost", false, "Enable boost mode for faster execution (--boost=false to explicitly disable)")
	cmd.BoolVar(&st.continuous, "continuous", false, "Run in continuous mode (--continuous=false to explicitly disable)")
	cmd.BoolVar(&st.detach, "detach", false, "Run in background without attaching to tmux")
	cmd.BoolVar(&st.detach, "d", false, "Shorthand for --detach")
	cmd.BoolVar(&st.force, "force", false, "Force start even if formation is already running")
	cmd.BoolVar(&st.force, "f", false, "Shorthand for --force")
	if err := cmd.Parse(args); err != nil {
		return nil, err
	}
	st.boostSet = cmd.Changed("boost")
	st.continuousSet = cmd.Changed("continuous")
	return st, nil
}

// runUp starts the formation (daemon + agents) and optionally attaches to tmux.
func runUp(args []string) error {
	flags, err := parseUpFlags(args)
	if err != nil {
		return err
	}

	maestroDir, err := requireMaestroDir("up")
	if err != nil {
		return err
	}

	cfg, err := model.LoadConfig(maestroDir)
	if err != nil {
		return fmt.Errorf("maestro up: load config: %w", err)
	}

	if err := setupTmuxSession("up", maestroDir, cfg); err != nil {
		return err
	}

	// Precheck: dispatcher creates per-worker worktrees from the base branch via
	// `git worktree add`. If the project is not a git repo, or the base branch
	// has no commits, every task fails before dispatch. Catch this up-front so
	// users get a clear, actionable message instead of opaque dispatch errors.
	// Only needed when worktree mode is enabled.
	if cfg.Worktree.Enabled {
		projectRoot := filepath.Dir(maestroDir)
		baseBranch := cfg.Worktree.EffectiveBaseBranch()
		if err := precheckGitRepo(projectRoot, baseBranch); err != nil {
			return &CLIError{Code: 1, Msg: fmt.Sprintf("maestro up: %v", err)}
		}
	}

	opts := formation.UpOptions{
		MaestroDir:    maestroDir,
		Config:        cfg,
		Boost:         flags.boost,
		Continuous:    flags.continuous,
		Force:         flags.force,
		BoostSet:      flags.boostSet,
		ContinuousSet: flags.continuousSet,
	}

	if err := formation.RunUp(opts); err != nil {
		// These errors are returned BEFORE any resource is created.
		// Running CleanupOnFailure for ErrSessionExists would kill a healthy
		// live formation; running it for preflight failures would add noisy
		// teardown errors when no tmux session or daemon exists yet.
		if skipRunUpCleanup(err) {
			return fmt.Errorf("maestro up: %w", err)
		}
		// Clean up any partially-created resources (tmux session, daemon)
		fmt.Fprintln(os.Stderr, "Cleaning up after setup failure...")
		if cleanupErr := formation.CleanupOnFailure(maestroDir); cleanupErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: cleanup encountered errors: %v\n", cleanupErr)
			return errors.Join(fmt.Errorf("maestro up: %w", err), fmt.Errorf("cleanup: %w", cleanupErr))
		}
		return fmt.Errorf("maestro up: %w", err)
	}

	printAttachHint(flags.detach)

	if !flags.detach {
		if os.Getenv("TMUX") != "" {
			return nil
		}
		if err := tmux.AttachSession(); err != nil {
			return fmt.Errorf("maestro up: attach: %w", err)
		}
	}
	return nil
}

func skipRunUpCleanup(err error) bool {
	return errors.Is(err, formation.ErrSessionExists) ||
		errors.Is(err, formation.ErrSandboxedLaunch) ||
		errors.Is(err, formation.ErrPreflightFailed)
}

// printAttachHint surfaces the attach commands so operators do not need to
// remember the per-instance tmux socket. The maestro session lives on
// `tmux -L <socket>`, not the default socket, so `tmux attach -t <session>`
// alone will not find it. We print both `maestro attach` (the friendly
// wrapper) and the raw tmux invocation in case the operator wants to script
// something on top of it.
func printAttachHint(detach bool) {
	session := tmux.GetSessionName()
	socket := tmux.GetTmuxSocket()
	insideTmux := os.Getenv("TMUX") != ""

	switch {
	case insideTmux:
		fmt.Printf("Already inside tmux. Switch with: maestro attach  (or: tmux switch-client -t %s)\n", session)
	case detach:
		if socket != "" {
			fmt.Printf("Attach with: maestro attach  (or: tmux -L %s attach -t %s)\n", socket, session)
		} else {
			fmt.Printf("Attach with: maestro attach  (or: tmux attach -t %s)\n", session)
		}
	}
}

// runAttach attaches the current terminal to the maestro tmux session for the
// project rooted at the discovered .maestro/ directory. Each maestro instance
// runs on its own tmux server (per-instance socket), so a bare
// `tmux attach -t maestro-<projectName>` from outside this command would miss
// the session — this wrapper resolves the right `-L <socket>` automatically.
func runAttach(args []string) error {
	cmd := NewCommand("maestro attach", "maestro attach")
	if err := cmd.Parse(args); err != nil {
		return err
	}

	maestroDir, err := requireMaestroDir("attach")
	if err != nil {
		return err
	}

	cfg, err := model.LoadConfig(maestroDir)
	if err != nil {
		return fmt.Errorf("maestro attach: load config: %w", err)
	}
	if err := setupTmuxSession("attach", maestroDir, cfg); err != nil {
		return err
	}

	if !tmux.SessionExists() {
		return &CLIError{Code: 1, Msg: fmt.Sprintf(
			"maestro attach: session %q not found on socket %q\n  hint: run `maestro up` first",
			tmux.GetSessionName(), tmux.GetTmuxSocket(),
		)}
	}

	// Inside an existing tmux client, attach-session is a no-op (tmux refuses
	// to nest); use switch-client so the operator's window jumps to the
	// maestro session instead.
	if os.Getenv("TMUX") != "" {
		fmt.Printf("Already inside tmux. Switching client to: %s\n", tmux.GetSessionName())
		if err := tmux.SwitchClient(tmux.GetSessionName()); err != nil {
			return fmt.Errorf("maestro attach: switch-client: %w", err)
		}
		return nil
	}

	if err := tmux.AttachSession(); err != nil {
		return fmt.Errorf("maestro attach: %w", err)
	}
	return nil
}

// runDown gracefully shuts down the formation.
func runDown(args []string) error {
	cmd := NewCommand("maestro down", "maestro down")
	if err := cmd.Parse(args); err != nil {
		return err
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

// validateBranchName checks that a git branch name does not contain dangerous characters.
func validateBranchName(name string) error {
	if name == "" {
		return fmt.Errorf("branch name must not be empty")
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("branch name contains control characters")
		}
	}
	for _, bad := range []string{"..", "~", "^", ":", "\\", " ", "?", "*", "["} {
		if strings.Contains(name, bad) {
			return fmt.Errorf("branch name contains invalid character sequence %q", bad)
		}
	}
	if strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.Contains(name, "//") {
		return fmt.Errorf("branch name has invalid slash placement")
	}
	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("branch name must not start with a hyphen")
	}
	if strings.HasSuffix(name, ".lock") {
		return fmt.Errorf("branch name must not end with .lock")
	}
	if strings.HasSuffix(name, ".") {
		return fmt.Errorf("branch name must not end with a dot")
	}
	return nil
}
