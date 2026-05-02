package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/verification"
	"github.com/msageha/maestro_v2/internal/model"
)

// RealVerifyRunner executes command-scoped verification snapshots from
// `.maestro/state/verify/<command>.yaml`. If a snapshot is missing, it falls
// back to model.DefaultVerifyConfigForProject rather than a mutable global file.
//
// Categories run in deterministic order — Build → Lint → Typecheck → Test →
// Security → Performance — and each command runs sequentially. The first
// failure returns Passed=false with the failing command's tail output as the
// reason; remaining commands are not executed (fail-fast). This mirrors the
// expected behaviour of CI tooling and keeps daemon CPU bounded.
type RealVerifyRunner struct {
	maestroDir     string
	projectDir     string
	logger         *slog.Logger
	commandTimeout time.Duration
	maxOutputBytes int
	// runner is the function used to execute a verification command. Tests
	// override this; production uses execVerifyCommand, which executes argv
	// directly without a shell.
	runner func(ctx context.Context, dir, cmd string) (output string, exitCode int, err error)
	// ensembleVerifierMu guards ensembleVerifier so it can be set after
	// construction (PhaseCManager is built later in daemon startup) without
	// racing the verify-execution goroutines.
	ensembleVerifierMu sync.RWMutex
	// ensembleVerifier carries the configured Phase C-3 perspectives. When
	// non-nil, Run augments the verify.yaml-derived categories with any
	// perspective whose Commands aren't already represented and lets the
	// perspective Weight decide whether a failure is critical (>=1.0;
	// fail-fast) or advisory (<1.0; logged but does not fail the run). nil
	// preserves the legacy fail-fast behaviour for every category.
	ensembleVerifier *verification.Verifier
}

// defaultVerifyCommandTimeout is the per-command timeout. Most build/lint/test
// invocations finish far below this; the cap prevents a runaway invocation
// from blocking task progression indefinitely.
const defaultVerifyCommandTimeout = 5 * time.Minute

// defaultVerifyMaxOutputBytes caps how much stdout/stderr is retained for the
// failure reason. Reporting more would balloon the audit log without aiding
// triage; the operator can re-run the command locally for full output.
const defaultVerifyMaxOutputBytes = 4 * 1024

// NewRealVerifyRunner constructs a RealVerifyRunner anchored at maestroDir
// for verify snapshots/configuration and projectDir for command execution.
func NewRealVerifyRunner(maestroDir, projectDir string, logger *slog.Logger) *RealVerifyRunner {
	if logger == nil {
		logger = slog.Default()
	}
	return &RealVerifyRunner{
		maestroDir:     maestroDir,
		projectDir:     projectDir,
		logger:         logger,
		commandTimeout: defaultVerifyCommandTimeout,
		maxOutputBytes: defaultVerifyMaxOutputBytes,
		runner:         execVerifyCommand,
	}
}

// SetEnsembleVerifier wires the Phase C-3 ensemble verifier so its
// configured perspectives (security, performance, etc.) actually run
// during verify and contribute their per-perspective weight to the
// pass/fail decision.
//
// Wired by daemon startup *after* PhaseCManager construction. May be
// called with nil to detach the verifier (e.g., when extended_verification
// is reconfigured at reload). Safe to call concurrently with Run; the
// snapshot read inside Run is mutex-protected.
func (r *RealVerifyRunner) SetEnsembleVerifier(v *verification.Verifier) {
	r.ensembleVerifierMu.Lock()
	defer r.ensembleVerifierMu.Unlock()
	r.ensembleVerifier = v
}

// Run loads the command-scoped verify snapshot (or the project fallback) and
// executes each command sequentially under cmd.Dir = workingDir (or r.projectDir
// when workingDir is empty). The first failure short-circuits the run.
//
// workingDir is non-empty when the caller has resolved a task-specific path
// (worker worktree, integration worktree, project root for RunOnMain). The
// fallback to r.projectDir exists for legacy test paths that do not pass a
// per-task directory; production callers always pass a resolved value so
// that worker-uncommitted changes are visible to verify commands.
func (r *RealVerifyRunner) Run(ctx context.Context, taskID, commandID, workingDir string, expectedPaths []string) (VerifyOutcome, error) {
	return r.runFiltered(ctx, taskID, commandID, workingDir, expectedPaths, false)
}

// RunSkippingHeavyCategories executes verification but skips the heavy
// repo-wide categories (test, security, performance). Lightweight categories
// (build, lint, typecheck) still run so syntax/type errors are caught at every
// task. The caller (runVerifySecondPass) opts into this mode when the source
// task is intermediate within its phase — earlier sibling tasks may have left
// the worktree in a state that is correct for the eventual phase merge but
// would fail repo-wide tests asserting against the *final* phase state. The
// final task in the phase still runs the full verify, gating the phase merge.
func (r *RealVerifyRunner) RunSkippingHeavyCategories(ctx context.Context, taskID, commandID, workingDir string, expectedPaths []string) (VerifyOutcome, error) {
	return r.runFiltered(ctx, taskID, commandID, workingDir, expectedPaths, true)
}

// heavyVerifyCategoryNames is the set of categories deferred when an
// intermediate task triggers verify. They tend to assert end-state correctness
// (`go test ./...`, security audits, perf benchmarks) and are flaky against a
// half-applied phase. Lightweight categories must remain to catch broken
// syntax/types at every task.
var heavyVerifyCategoryNames = map[string]struct{}{
	"test":        {},
	"security":    {},
	"performance": {},
}

func (r *RealVerifyRunner) runFiltered(ctx context.Context, taskID, commandID, workingDir string, expectedPaths []string, skipHeavyCategories bool) (VerifyOutcome, error) {
	cfg, source, err := r.loadVerifyConfig(commandID)
	if err != nil {
		return VerifyOutcome{Passed: false, Reason: fmt.Sprintf("verify_config_invalid: %v", err)}, nil
	}
	if cfg.IsEmpty() {
		return VerifyOutcome{Passed: false, Reason: "verify_config_empty: verify config must contain at least one command"}, nil
	}

	runDir := workingDir
	if runDir == "" {
		runDir = r.projectDir
	}

	// Fail-fast on inaccessible runDir. When a worktree has been cleaned
	// up between dispatch and verify (e.g. fast-track stall cleanup races
	// a verify already in flight), every downstream `cmd.Run()` would fail
	// with `chdir … : no such file or directory` and the per-category
	// advisory path could end up returning Passed=true with
	// "passed_with_advisory_failures" — silently marking the task
	// completed even though no verify command actually executed. A hard
	// failure here surfaces the missing worktree as a real verify failure.
	if info, err := os.Stat(runDir); err != nil {
		return VerifyOutcome{
			Passed: false,
			Reason: fmt.Sprintf("verify_workdir_inaccessible path=%q: %v", runDir, err),
		}, nil
	} else if !info.IsDir() {
		return VerifyOutcome{
			Passed: false,
			Reason: fmt.Sprintf("verify_workdir_not_directory path=%q", runDir),
		}, nil
	}

	// expected_paths is advisory: changes outside the declared paths are
	// logged for operator review but do not fail verify. The constraint
	// is software-engineering specific (commit-boundary policy for
	// refactor tasks) and produces false-positive failures on legitimate
	// fixes that touch ancillary files; for research / documentation /
	// polyglot orchestration the constraint is inapplicable. Commit-
	// boundary enforcement still happens at commit_policy time when the
	// worktree is staged for integration.
	if len(expectedPaths) > 0 {
		changed, err := gitChangedFiles(ctx, runDir)
		if err != nil {
			r.logger.Warn("verify_expected_paths_check_skipped",
				"task_id", taskID, "command_id", commandID, "error", err.Error(),
				"reason", "git_status_unavailable")
		} else if outside := filesOutsideExpectedPaths(changed, expectedPaths); len(outside) > 0 {
			r.logger.Warn("verify_expected_paths_advisory",
				"task_id", taskID, "command_id", commandID,
				"files_outside_expected", outside, "expected_paths", expectedPaths,
				"note", "advisory only; verify proceeds. Commit-policy will enforce final boundary.")
		}
	}

	categories := r.buildVerifyCategories(cfg)
	skippedHeavy := 0
	if skipHeavyCategories {
		filtered := categories[:0]
		for _, cat := range categories {
			if _, isHeavy := heavyVerifyCategoryNames[cat.name]; isHeavy {
				skippedHeavy++
				continue
			}
			filtered = append(filtered, cat)
		}
		categories = filtered
	}

	r.logger.Info("verify_runner_start",
		"task_id", taskID, "command_id", commandID,
		"working_dir", runDir, "config_source", source,
		"category_count", len(categories), "skipped_heavy", skippedHeavy,
		"intermediate_task", skipHeavyCategories)

	var advisoryFailures []string

	for _, cat := range categories {
		categoryFailed := false
		for _, cmd := range cat.cmds {
			cmd = strings.TrimSpace(cmd)
			if cmd == "" {
				continue
			}
			if err := ctx.Err(); err != nil {
				return VerifyOutcome{Passed: false, Reason: fmt.Sprintf("verify_aborted: %v", err)}, err
			}

			cmdCtx, cancel := context.WithTimeout(ctx, r.commandTimeout)
			output, exitCode, runErr := r.runner(cmdCtx, runDir, cmd)
			cancel()

			timedOut := errors.Is(cmdCtx.Err(), context.DeadlineExceeded)
			failed := timedOut || runErr != nil || exitCode != 0

			// Mid-run worktree disappearance check: when a worktree
			// cleanup races a still-running verify command, the
			// subprocess inherits an unlinked cwd and commands like
			// `uvx pip-audit` fail with FileNotFoundError on os.getcwd().
			// Re-stat the workdir after each failed command so the audit
			// log captures the environmental cause instead of attributing
			// the noise to a real test failure.
			//
			// Earlier this branch returned Passed=true under the assumption
			// that the publish gate had already advanced past the point
			// where verify mattered. That assumption was wrong — the gate
			// (collectWorktreePublishAndCleanup) now blocks publish while
			// any TaskStates entry is non-terminal, so swallowing a
			// verify failure here actively defeats the gate by marking the
			// task as completed in state without ever observing whether the
			// expected verify commands passed. Routing the failure as a
			// verify failure with `verify_runner_workdir_inaccessible` as
			// the reason makes the §2.1 pipeline send the task to
			// repair_pending instead, which (a) preserves the gate (state
			// is non-terminal, so publish stays blocked), (b) lets the
			// daemon-side repair retry recover when the cleanup was a
			// transient race, and (c) keeps the audit log truthful: verify
			// did NOT pass, the runner just could not observe the outcome.
			//
			// We still only short-circuit when the command itself failed
			// AND the workdir is gone — a passing command proves the dir
			// was alive at least until completion, so continuing to run
			// subsequent categories is safe.
			if failed {
				if _, statErr := os.Stat(runDir); statErr != nil {
					r.logger.Warn("verify_runner_workdir_disappeared_during_run",
						"task_id", taskID, "command_id", commandID,
						"category", cat.name, "command", cmd,
						"working_dir", runDir, "stat_error", statErr,
						"hint", "worktree cleanup likely raced an in-flight verify command; "+
							"routing as environmental verify failure so the publish gate stays "+
							"blocked while repair_pending decides whether to retry")
					reason := fmt.Sprintf(
						"verify_runner_workdir_inaccessible category=%s command=%q working_dir=%q stat_error=%v",
						cat.name, cmd, runDir, statErr,
					)
					return VerifyOutcome{Passed: false, Reason: reason}, nil
				}
			}

			if !failed {
				r.logger.Debug("verify_runner_command_passed",
					"task_id", taskID, "command_id", commandID,
					"category", cat.name, "command", cmd, "weight", cat.weight)
				continue
			}

			var reason string
			if timedOut {
				reason = fmt.Sprintf("verify_timeout category=%s command=%q timeout=%s", cat.name, cmd, r.commandTimeout)
			} else {
				tail := tailBytes(output, r.maxOutputBytes)
				reason = fmt.Sprintf("verify_failed category=%s command=%q exit=%d output_tail=%q",
					cat.name, cmd, exitCode, tail)
				if runErr != nil {
					reason = fmt.Sprintf("%s err=%v", reason, runErr)
				}
			}

			if cat.advisory {
				// Advisory perspective (extended_verification weight < 1.0):
				// log + record but do not fail the run. Move on to the next
				// command in this category so all advisory checks get a
				// chance to surface issues.
				r.logger.Warn("verify_runner_advisory_failed",
					"task_id", taskID, "command_id", commandID,
					"category", cat.name, "command", cmd,
					"weight", cat.weight, "exit", exitCode, "reason", reason)
				advisoryFailures = append(advisoryFailures, reason)
				categoryFailed = true
				continue
			}

			r.logger.Warn("verify_runner_command_failed",
				"task_id", taskID, "command_id", commandID,
				"category", cat.name, "command", cmd, "exit", exitCode, "weight", cat.weight)
			return VerifyOutcome{Passed: false, Reason: reason}, nil
		}
		if !categoryFailed {
			r.logger.Debug("verify_runner_category_passed",
				"task_id", taskID, "command_id", commandID,
				"category", cat.name, "weight", cat.weight, "advisory", cat.advisory)
		}
	}

	if len(advisoryFailures) > 0 {
		r.logger.Info("verify_runner_passed_with_advisory_failures",
			"task_id", taskID, "command_id", commandID,
			"advisory_failure_count", len(advisoryFailures),
			"first", tailBytes(advisoryFailures[0], r.maxOutputBytes))
	} else {
		r.logger.Info("verify_runner_passed",
			"task_id", taskID, "command_id", commandID, "expected_paths", len(expectedPaths))
	}
	return VerifyOutcome{Passed: true}, nil
}

// verifyCategory is one execution unit inside RealVerifyRunner.Run.
// advisory=true marks a category whose failure must NOT abort the run;
// the caller treats it as informational (extended_verification weight
// < 1.0). weight is preserved for log correlation only.
type verifyCategory struct {
	name     string
	cmds     []string
	weight   float64
	advisory bool
}

// buildVerifyCategories converts the verify.yaml-derived configuration
// into the verifyCategory slice consumed by the runner. Every category
// listed in verify.yaml runs at the critical weight (1.0): verify.yaml
// is the single source of truth — if a category is listed, it is
// critical; if absent, it does not run.
func (r *RealVerifyRunner) buildVerifyCategories(cfg *model.VerifyConfig) []verifyCategory {
	cfgCategories := []struct {
		name string
		cmds []string
	}{
		{"build", cfg.Build},
		{"lint", cfg.Lint},
		{"typecheck", cfg.Typecheck},
		{"test", cfg.Test},
		{"security", cfg.Security},
		{"performance", cfg.Performance},
	}

	out := make([]verifyCategory, 0, len(cfgCategories))
	for _, c := range cfgCategories {
		if len(c.cmds) == 0 {
			continue
		}
		out = append(out, verifyCategory{
			name:     c.name,
			cmds:     c.cmds,
			weight:   defaultCategoryCriticalWeight,
			advisory: false,
		})
	}
	return out
}

// defaultCategoryCriticalWeight is the threshold at which a category
// switches from advisory to fail-fast. Matches verification.Verifier's
// Aggregate semantics where weight >= 1.0 perspectives gate the overall
// pass/fail decision.
const defaultCategoryCriticalWeight = 1.0

func (r *RealVerifyRunner) loadVerifyConfig(commandID string) (*model.VerifyConfig, string, error) {
	if commandID != "" {
		path := verifySnapshotPath(r.maestroDir, commandID)
		cfg, err := model.LoadVerifyConfig(path)
		if err == nil {
			return cfg, path, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, path, err
		}
		return model.DefaultVerifyConfigForProject(r.projectDir), "project_default", nil
	}
	globalPath := filepath.Join(r.maestroDir, "verify.yaml")
	cfg, err := model.LoadOrDefaultVerifyConfigForProject(r.projectDir, globalPath)
	if err != nil {
		return nil, globalPath, err
	}
	return cfg, globalPath, nil
}

func gitChangedFiles(ctx context.Context, dir string) ([]string, error) {
	// `-uall` is required so untracked *files* inside a freshly created
	// directory are listed individually (e.g. `notes/note_b.txt`). With the
	// default `-unormal`, git collapses an untracked directory to a single
	// entry (`notes/`), which then false-fails the expected_paths check
	// because the matcher compares `notes` against `notes/note_b.txt` and
	// rejects the directory as "outside expected paths". This was a 100%
	// reproducible verify failure for any task that creates new directories.
	c := exec.CommandContext(ctx, "git", "status", "--porcelain", "-uall", "-z") //nolint:gosec // fixed argv, dir supplied by daemon worktree resolver
	c.Dir = dir
	out, err := c.Output()
	if err != nil {
		return nil, err
	}
	parts := strings.Split(string(out), "\x00")
	files := make([]string, 0, len(parts))
	for i := 0; i < len(parts); i++ {
		part := parts[i]
		if part == "" {
			continue
		}
		if len(part) < 4 {
			continue
		}
		code := part[:2]
		path := strings.TrimSpace(part[3:])
		if path != "" {
			files = append(files, path)
		}
		if code[0] == 'R' || code[0] == 'C' {
			i++
			if i < len(parts) && parts[i] != "" {
				files = append(files, strings.TrimSpace(parts[i]))
			}
		}
	}
	return files, nil
}

func filesOutsideExpectedPaths(files, expectedPaths []string) []string {
	var outside []string
	for _, file := range files {
		if !verifyPathAllowed(file, expectedPaths) {
			outside = append(outside, file)
		}
	}
	return outside
}

// dependencyManifestBasenames lists the basenames of dependency-manifest /
// lockfile artefacts that every package manager touches as a side effect of
// installing or upgrading a library. They are unconditionally admitted
// alongside expected_paths so that a routine `npm install -D vitest`,
// `pip install ...`, `cargo add ...` etc. does not flunk verify with an
// expected_paths_violation and force the planner into an extra repair
// cycle just to widen the path declaration.
//
// Inclusion criteria: the file MUST be (a) machine-generated by the
// language's package manager, (b) safe-to-commit (not a credential or
// developer-local cache), and (c) a routinely-changing artefact of
// dependency operations. Source manifests like pyproject.toml are also
// listed because they are commonly edited alongside the lockfile.
var dependencyManifestBasenames = map[string]struct{}{
	// Node / npm / yarn / pnpm
	"package.json":        {},
	"package-lock.json":   {},
	"npm-shrinkwrap.json": {},
	"yarn.lock":           {},
	"pnpm-lock.yaml":      {},
	// Go
	"go.mod": {},
	"go.sum": {},
	// Rust
	"Cargo.toml": {},
	"Cargo.lock": {},
	// Python (uv, poetry, pip, pipenv)
	"pyproject.toml":   {},
	"uv.lock":          {},
	"poetry.lock":      {},
	"Pipfile":          {},
	"Pipfile.lock":     {},
	"requirements.txt": {},
	// Ruby
	"Gemfile":      {},
	"Gemfile.lock": {},
	// PHP
	"composer.json": {},
	"composer.lock": {},
	// Elixir
	"mix.exs":  {},
	"mix.lock": {},
}

// isDependencyManifest reports whether file (a normalised slash-path) is a
// dependency-manifest artefact admitted regardless of expected_paths.
// Match is on basename only — manifests live at the project root for
// every package manager listed here, and matching by basename catches
// monorepo workspaces (e.g. apps/web/package.json) without enumerating
// every nesting depth in the allow list.
func isDependencyManifest(file string) bool {
	_, ok := dependencyManifestBasenames[path.Base(file)]
	return ok
}

func verifyPathAllowed(file string, expectedPaths []string) bool {
	file = filepath.ToSlash(filepath.Clean(strings.TrimSpace(file)))
	if isDependencyManifest(file) {
		return true
	}
	for _, exp := range expectedPaths {
		exp = filepath.ToSlash(filepath.Clean(strings.TrimSpace(exp)))
		if exp == "" {
			continue
		}
		if exp == "." {
			return true
		}
		exp = strings.TrimSuffix(exp, "/")
		if file == exp || strings.HasPrefix(file, exp+"/") {
			return true
		}
	}
	return false
}

// execVerifyCommand runs cmd as argv without invoking a shell, with cmd.Dir =
// dir, capturing the combined output. Returns (output, exitCode, error).
// exitCode is 0 on success, non-zero on command failure, and -1 when the
// process could not be started.
func execVerifyCommand(ctx context.Context, dir, cmd string) (string, int, error) {
	parsed, err := model.ParseVerifyCommand(cmd)
	if err != nil {
		return "", -1, err
	}
	c := exec.CommandContext(ctx, parsed.Args[0], parsed.Args[1:]...) //nolint:gosec // executable and args come from validated verify config and are executed without a shell
	c.Dir = dir
	if len(parsed.Env) > 0 {
		c.Env = append(os.Environ(), parsed.Env...)
	}
	var buf bytes.Buffer
	c.Stdout = &buf
	c.Stderr = &buf
	err = c.Run()
	output := buf.String()
	if err == nil {
		return output, 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return output, exitErr.ExitCode(), nil
	}
	return output, -1, err
}

// tailBytes returns the last n bytes of s. When s is shorter than n the full
// string is returned. The function never returns mid-rune-broken output.
func tailBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[len(s)-n:]
	// Trim partial UTF-8 prefix so log readers do not see a replacement char
	// at the head of the tail. RuneStart bytes have the form 0xxxxxxx or
	// 11xxxxxx; we advance past any 10xxxxxx continuation bytes.
	for i := 0; i < 4 && i < len(cut); i++ {
		if cut[0]&0xC0 != 0x80 {
			break
		}
		cut = cut[1:]
	}
	return cut
}
