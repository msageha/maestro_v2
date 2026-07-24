package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

// Runtime probe guardrails. Every external-runtime probe (`<cli> --version`)
// runs non-interactively (stdin is the null device), under a hard timeout,
// and with its captured output capped — a misbehaving CLI that opens an
// interactive prompt or floods stdout can neither hang the preflight nor
// blow up memory/log size. These caps guard diagnostics only; they are not
// applied to worker task output (see commit_policy_removed: worker results
// are committed verbatim).
const (
	// DefaultRuntimeProbeTimeout bounds a single version probe. Version
	// flags return in well under a second on a healthy install; 10s leaves
	// headroom for cold starts (e.g. node-based CLIs on first run).
	DefaultRuntimeProbeTimeout = 10 * time.Second
	// DefaultRuntimeProbeMaxOutputBytes caps how much probe output is
	// retained. Anything beyond the cap is counted and discarded.
	DefaultRuntimeProbeMaxOutputBytes = 8 * 1024
)

// RuntimeAuthStatus is the best-effort authentication verdict for a runtime.
// There is deliberately no "fail" value: credential stores differ per
// runtime version and OS (e.g. claude-code keeps OAuth tokens in the macOS
// keychain, which is not inspectable from here), so the strongest negative
// signal preflight can honestly emit is "unknown" — a warning, never a hard
// stop. Only binary absence is a certain failure.
type RuntimeAuthStatus string

// RuntimeAuthStatus values.
const (
	RuntimeAuthOK      RuntimeAuthStatus = "ok"
	RuntimeAuthUnknown RuntimeAuthStatus = "unknown"
)

// RuntimeCheckStatus is the aggregate verdict for one runtime check.
type RuntimeCheckStatus string

// RuntimeCheckStatus values, ordered by severity (fail > warn > ok).
const (
	RuntimeCheckOK   RuntimeCheckStatus = "ok"
	RuntimeCheckWarn RuntimeCheckStatus = "warn"
	RuntimeCheckFail RuntimeCheckStatus = "fail"
)

// RuntimeCheckResult reports the outcome of preflighting a single runtime.
type RuntimeCheckResult struct {
	Runtime string `json:"runtime"`
	// Agents lists the agent IDs (orchestrator / planner / workerN) whose
	// configured model resolves to this runtime.
	Agents  []string `json:"agents,omitempty"`
	Command string   `json:"command"`

	BinaryPath  string `json:"binary_path,omitempty"`
	BinaryError string `json:"binary_error,omitempty"`

	Version      string `json:"version,omitempty"`
	VersionError string `json:"version_error,omitempty"`

	AuthStatus RuntimeAuthStatus `json:"auth_status,omitempty"`
	AuthDetail string            `json:"auth_detail,omitempty"`

	// Status is the aggregate verdict: fail when the binary is missing
	// (certain failure), warn when the version probe failed or auth is
	// unknown (best-effort signals), ok otherwise.
	Status RuntimeCheckStatus `json:"status"`
}

// RuntimePreflight verifies that a configured agent runtime is launchable
// BEFORE any formation resource is created: binary on PATH (hard check),
// version probe (best-effort, timeout + output cap), and credential
// presence (best-effort, filesystem/env inspection only — no subprocess).
//
// It is the early-diagnosis front end for the existing late-failure
// handling; the launch-time LookPath in launcher.go and the pane
// terminal-error fast-fail (queue_scan_blocked_pane.go) remain in place as
// the authoritative backstops.
//
// The function fields exist so tests (and callers in other packages) can
// inject fakes instead of touching the host PATH, env, or forking real
// processes. NewRuntimePreflight wires the production defaults.
type RuntimePreflight struct {
	// LookPath resolves a command name on PATH. Default: exec.LookPath.
	LookPath func(file string) (string, error)
	// RunCommand executes a probe and returns its combined output.
	// Implementations must honour ctx cancellation and must not attach the
	// caller's stdin (probes have to be non-interactive). Default:
	// runProbeCommand, which also applies MaxOutputBytes.
	RunCommand func(ctx context.Context, maxOutputBytes int, name string, args ...string) ([]byte, error)
	// Getenv reads an environment variable. Default: os.Getenv.
	Getenv func(key string) string
	// UserHomeDir resolves the user's home directory. Default: os.UserHomeDir.
	UserHomeDir func() (string, error)
	// Timeout bounds each version probe. Default: DefaultRuntimeProbeTimeout.
	Timeout time.Duration
	// MaxOutputBytes caps retained probe output. Default:
	// DefaultRuntimeProbeMaxOutputBytes.
	MaxOutputBytes int
}

// NewRuntimePreflight returns a RuntimePreflight with production defaults.
func NewRuntimePreflight() *RuntimePreflight {
	return &RuntimePreflight{
		LookPath:       exec.LookPath,
		RunCommand:     runProbeCommand,
		Getenv:         os.Getenv,
		UserHomeDir:    os.UserHomeDir,
		Timeout:        DefaultRuntimeProbeTimeout,
		MaxOutputBytes: DefaultRuntimeProbeMaxOutputBytes,
	}
}

// runtimeVersionArgs maps a runtime to the argv of its non-interactive
// version probe. All currently registered runtimes answer `--version` and
// exit; a runtime without an entry here is reported as "version probe not
// supported" rather than probed blind (an unknown CLI given a guessed flag
// could drop into an interactive REPL).
var runtimeVersionArgs = map[string][]string{
	model.RuntimeClaudeCode: {"--version"},
	model.RuntimeCodex:      {"--version"},
	model.RuntimeGemini:     {"--version"},
}

// RuntimeCommandName returns the CLI command a runtime is launched with,
// resolved from the same RuntimeDef registry the launcher uses so preflight
// and launch can never disagree about which binary matters.
func RuntimeCommandName(runtime string) (string, error) {
	rl := NewRuntimeLauncher()
	if runtime == "" {
		runtime = rl.defaultRuntime
	}
	def, ok := rl.runtimes[runtime]
	if !ok {
		return "", fmt.Errorf("unknown runtime %q", runtime)
	}
	return def.Command, nil
}

// CheckBinary resolves the runtime's CLI on PATH. This is the only check
// whose failure is treated as certain (RuntimeCheckFail): a missing binary
// can never launch, whereas version/auth probes are best-effort.
func (p *RuntimePreflight) CheckBinary(runtime string) (cmdName, path string, err error) {
	cmdName, err = RuntimeCommandName(runtime)
	if err != nil {
		return "", "", err
	}
	path, err = p.LookPath(cmdName)
	if err != nil {
		return cmdName, "", fmt.Errorf("resolve %s executable: %w", cmdName, err)
	}
	return cmdName, path, nil
}

// ProbeVersion runs the runtime's version probe non-interactively with the
// configured timeout and output cap, returning the first non-empty output
// line. Failures (missing probe definition, timeout, non-zero exit) are
// returned as errors for the caller to downgrade to a warning.
func (p *RuntimePreflight) ProbeVersion(ctx context.Context, runtime, cmdName string) (string, error) {
	args, ok := runtimeVersionArgs[runtime]
	if !ok {
		return "", fmt.Errorf("version probe not supported for runtime %q", runtime)
	}
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	out, err := p.RunCommand(ctx, p.MaxOutputBytes, cmdName, args...)
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("%s %s timed out after %s (probe killed; output capped at %d bytes)",
				cmdName, strings.Join(args, " "), p.Timeout, p.MaxOutputBytes)
		}
		return "", fmt.Errorf("%s %s: %w (output: %s)", cmdName, strings.Join(args, " "), err, firstLine(out))
	}
	v := versionLine(out)
	if v == "" {
		return "", fmt.Errorf("%s %s produced no output", cmdName, strings.Join(args, " "))
	}
	return v, nil
}

// CheckAuth inspects credential env vars and on-disk credential files for
// the runtime. Best-effort by design: a positive hit yields RuntimeAuthOK,
// anything else yields RuntimeAuthUnknown with a remediation hint. It never
// spawns a subprocess, so it is cheap enough to run on every `maestro up`.
func (p *RuntimePreflight) CheckAuth(runtime string) (RuntimeAuthStatus, string) {
	switch runtime {
	case model.RuntimeClaudeCode:
		if v := p.firstEnvSet("ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"); v != "" {
			return RuntimeAuthOK, v + " is set"
		}
		if path, ok := p.homeFileExists(".claude", ".credentials.json"); ok {
			return RuntimeAuthOK, "credentials found: " + path
		}
		return RuntimeAuthUnknown, "no credential env var (ANTHROPIC_API_KEY / CLAUDE_CODE_OAUTH_TOKEN) and no ~/.claude/.credentials.json; " +
			"claude may still be logged in via the OS keychain — run `claude` once to confirm"
	case model.RuntimeCodex:
		if v := p.firstEnvSet("OPENAI_API_KEY"); v != "" {
			return RuntimeAuthOK, v + " is set"
		}
		codexHome := p.Getenv("CODEX_HOME")
		if codexHome != "" {
			if fileExists(filepath.Join(codexHome, "auth.json")) {
				return RuntimeAuthOK, "credentials found: " + filepath.Join(codexHome, "auth.json")
			}
		} else if path, ok := p.homeFileExists(".codex", "auth.json"); ok {
			return RuntimeAuthOK, "credentials found: " + path
		}
		return RuntimeAuthUnknown, "no OPENAI_API_KEY and no auth.json under CODEX_HOME (~/.codex); " +
			"codex is likely not logged in — run `codex login`"
	case model.RuntimeGemini:
		if v := p.firstEnvSet("GEMINI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_APPLICATION_CREDENTIALS"); v != "" {
			return RuntimeAuthOK, v + " is set"
		}
		if path, ok := p.homeFileExists(".gemini", "oauth_creds.json"); ok {
			return RuntimeAuthOK, "credentials found: " + path
		}
		return RuntimeAuthUnknown, "no credential env var (GEMINI_API_KEY / GOOGLE_API_KEY / GOOGLE_APPLICATION_CREDENTIALS) " +
			"and no ~/.gemini/oauth_creds.json; run `gemini` once and complete the login flow"
	default:
		return RuntimeAuthUnknown, fmt.Sprintf("no auth heuristic for runtime %q", runtime)
	}
}

// Check runs the full binary + version + auth preflight for one runtime and
// aggregates the verdict. A missing binary short-circuits: version and auth
// probes are skipped because they would only add noise about a CLI that
// cannot launch anyway.
func (p *RuntimePreflight) Check(ctx context.Context, runtime string, agents []string) RuntimeCheckResult {
	res := RuntimeCheckResult{Runtime: runtime, Agents: agents, Status: RuntimeCheckOK}

	cmdName, path, err := p.CheckBinary(runtime)
	res.Command = cmdName
	if err != nil {
		res.BinaryError = err.Error()
		res.Status = RuntimeCheckFail
		return res
	}
	res.BinaryPath = path

	if v, err := p.ProbeVersion(ctx, runtime, cmdName); err != nil {
		res.VersionError = err.Error()
		res.Status = RuntimeCheckWarn
	} else {
		res.Version = v
	}

	authStatus, detail := p.CheckAuth(runtime)
	res.AuthStatus = authStatus
	res.AuthDetail = detail
	if authStatus != RuntimeAuthOK && res.Status == RuntimeCheckOK {
		res.Status = RuntimeCheckWarn
	}
	return res
}

// firstEnvSet returns the name of the first env var in keys that is
// non-empty, or "" when none is set. Only the NAME is returned — credential
// values must never reach logs or terminal output.
func (p *RuntimePreflight) firstEnvSet(keys ...string) string {
	for _, k := range keys {
		if p.Getenv(k) != "" {
			return k
		}
	}
	return ""
}

// homeFileExists reports whether <home>/<elem...> exists, returning the
// resolved path for diagnostics.
func (p *RuntimePreflight) homeFileExists(elem ...string) (string, bool) {
	home, err := p.UserHomeDir()
	if err != nil || home == "" {
		return "", false
	}
	path := filepath.Join(append([]string{home}, elem...)...)
	return path, fileExists(path)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// firstLine returns the first non-empty, whitespace-trimmed line of out.
func firstLine(out []byte) string {
	for _, line := range strings.Split(string(out), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// versionNumberPattern matches a dotted version number (e.g. "0.125.0",
// "2.1.190") anywhere in a line.
var versionNumberPattern = regexp.MustCompile(`\d+\.\d+`)

// versionLine extracts the version line from probe output: the first line
// containing a dotted version number, falling back to the first non-empty
// line. The heuristic matters in practice — some CLIs prepend startup
// warnings to their --version output (observed with codex emitting a
// sandbox PATH-alias warning before the version string), and reporting the
// warning as "the version" would mislead operators.
func versionLine(out []byte) string {
	for _, line := range strings.Split(string(out), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && versionNumberPattern.MatchString(trimmed) {
			return trimmed
		}
	}
	return firstLine(out)
}

// runProbeCommand is the production RunCommand: it executes the probe with
// stdin detached (the null device — an interactive prompt reads EOF instead
// of hanging), honours ctx cancellation via exec.CommandContext, and caps
// the retained combined output at maxOutputBytes.
func runProbeCommand(ctx context.Context, maxOutputBytes int, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // name resolves from the fixed RuntimeDef registry; args are code literals
	w := &cappedWriter{max: maxOutputBytes}
	// Stdin is left nil: os/exec then connects the child to the null
	// device, so a CLI that unexpectedly prompts reads EOF immediately.
	cmd.Stdout = w
	cmd.Stderr = w
	err := cmd.Run()
	return w.Bytes(), err
}

// cappedWriter retains the first max bytes written and silently discards
// the rest, so a probe that floods output cannot grow memory unboundedly.
type cappedWriter struct {
	max int
	buf []byte
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	if remaining := w.max - len(w.buf); remaining > 0 {
		if len(p) > remaining {
			w.buf = append(w.buf, p[:remaining]...)
		} else {
			w.buf = append(w.buf, p...)
		}
	}
	// Report full consumption so the child never sees a short write.
	return len(p), nil
}

func (w *cappedWriter) Bytes() []byte { return w.buf }

// ConfiguredRuntime pairs a runtime with the agent IDs whose configured
// model resolves to it.
type ConfiguredRuntime struct {
	Name   string
	Agents []string
}

// ConfiguredRuntimes returns the deduplicated, launch-order list of runtimes
// referenced by the agents config: orchestrator, planner, then
// worker1..workerN. Model → runtime resolution mirrors formation
// (resolveModel + model.ParseRuntimeFromModel), including boost forcing all
// workers onto opus (claude-code) and the "sonnet" fallback for empty
// models, so preflight validates exactly the set of CLIs the formation will
// launch.
func ConfiguredRuntimes(cfg model.Config) []ConfiguredRuntime {
	type entry struct {
		agentID   string
		modelName string
	}
	entries := []entry{
		{"orchestrator", effectiveAgentModel(cfg.Agents.Orchestrator.Model)},
		{"planner", effectiveAgentModel(cfg.Agents.Planner.Model)},
	}
	workerCount := max(cfg.Agents.Workers.Count, 1)
	for i := 1; i <= workerCount; i++ {
		workerID := fmt.Sprintf("worker%d", i)
		entries = append(entries, entry{workerID, model.ResolveWorkerModel(workerID, cfg)})
	}

	var runtimes []ConfiguredRuntime
	index := make(map[string]int)
	for _, e := range entries {
		runtime, _ := model.ParseRuntimeFromModel(e.modelName)
		if i, ok := index[runtime]; ok {
			runtimes[i].Agents = append(runtimes[i].Agents, e.agentID)
			continue
		}
		index[runtime] = len(runtimes)
		runtimes = append(runtimes, ConfiguredRuntime{Name: runtime, Agents: []string{e.agentID}})
	}
	return runtimes
}

// effectiveAgentModel mirrors formation's resolveModel fallback for the
// orchestrator / planner roles ("sonnet" when unset).
func effectiveAgentModel(m string) string {
	if m != "" {
		return m
	}
	return "sonnet"
}
