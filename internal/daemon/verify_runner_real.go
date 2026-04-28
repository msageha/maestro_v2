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

func (r *RealVerifyRunner) snapshotEnsembleVerifier() *verification.Verifier {
	r.ensembleVerifierMu.RLock()
	defer r.ensembleVerifierMu.RUnlock()
	return r.ensembleVerifier
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
	if len(expectedPaths) > 0 {
		changed, err := gitChangedFiles(ctx, runDir)
		if err != nil {
			return VerifyOutcome{Passed: false, Reason: fmt.Sprintf("verify_expected_paths_check_failed: %v", err)}, nil
		}
		if outside := filesOutsideExpectedPaths(changed, expectedPaths); len(outside) > 0 {
			return VerifyOutcome{
				Passed: false,
				Reason: fmt.Sprintf("verify_expected_paths_violation files=%q expected_paths=%q", outside, expectedPaths),
			}, nil
		}
	}

	categories := r.buildVerifyCategories(cfg)

	r.logger.Info("verify_runner_start",
		"task_id", taskID, "command_id", commandID,
		"working_dir", runDir, "config_source", source,
		"category_count", len(categories))

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

// buildVerifyCategories merges the verify.yaml-derived configuration with
// the EnsembleVerifier perspectives so extended_verification settings
// actually influence the run. The merge is one-way (perspectives augment
// the loaded config; verify.yaml never replaces the perspective set):
//
//   - Categories already populated by cfg keep their commands but pick up
//     the perspective's weight if one is configured. This lets an
//     operator-authored verify.yaml tighten or relax criticality without
//     changing commands.
//   - Categories absent from cfg but present in perspectives are added
//     with the perspective's commands. This is what makes
//     extended_verification.security_check / performance_bench actually
//     run when a planner-generated snapshot omits those categories.
//   - When no EnsembleVerifier is wired, every cfg category runs at the
//     legacy weight 1.0 (critical) so behaviour matches the pre-wiring
//     code path.
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

	weights := perspectiveWeightsByName(r.snapshotEnsembleVerifier())

	out := make([]verifyCategory, 0, len(cfgCategories)+2)
	covered := make(map[string]bool, len(cfgCategories))
	for _, c := range cfgCategories {
		if len(c.cmds) == 0 {
			// Empty category in cfg does NOT count as "covered": that
			// would suppress the EnsembleVerifier perspective for the
			// same name even though the verify config provided no
			// command of its own. extended_verification's whole point
			// is to fill gaps left by the snapshot, so we must leave
			// the slot open for the perspective merge below.
			continue
		}
		covered[c.name] = true
		weight, hasWeight := weights[c.name]
		if !hasWeight {
			weight = defaultCategoryCriticalWeight
		}
		out = append(out, verifyCategory{
			name:     c.name,
			cmds:     c.cmds,
			weight:   weight,
			advisory: weight < defaultCategoryCriticalWeight,
		})
	}

	// Append perspectives whose category isn't already represented in cfg.
	// Iterating Perspectives() rather than the weights map preserves the
	// configured order and lets us pick up the perspective's Commands.
	if v := r.snapshotEnsembleVerifier(); v != nil {
		for _, p := range v.Perspectives() {
			if covered[p.Name] {
				continue
			}
			if len(p.Commands) == 0 {
				continue
			}
			out = append(out, verifyCategory{
				name:     p.Name,
				cmds:     p.Commands,
				weight:   p.Weight,
				advisory: p.Weight < defaultCategoryCriticalWeight,
			})
		}
	}
	return out
}

// defaultCategoryCriticalWeight is the threshold at which a category
// switches from advisory to fail-fast. Matches verification.Verifier's
// Aggregate semantics where weight >= 1.0 perspectives gate the overall
// pass/fail decision.
const defaultCategoryCriticalWeight = 1.0

// perspectiveWeightsByName returns a name → weight lookup for the
// EnsembleVerifier's configured perspectives. Returns nil for an absent
// verifier so callers can use a single shared zero-value path.
func perspectiveWeightsByName(v *verification.Verifier) map[string]float64 {
	if v == nil {
		return nil
	}
	perspectives := v.Perspectives()
	out := make(map[string]float64, len(perspectives))
	for _, p := range perspectives {
		out[p.Name] = p.Weight
	}
	return out
}

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
	c := exec.CommandContext(ctx, "git", "status", "--porcelain", "-z") //nolint:gosec // fixed argv, dir supplied by daemon worktree resolver
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

func verifyPathAllowed(file string, expectedPaths []string) bool {
	file = filepath.ToSlash(filepath.Clean(strings.TrimSpace(file)))
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
