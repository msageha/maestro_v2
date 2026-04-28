package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/msageha/maestro_v2/internal/model"
)

func TestRuntimeLauncher_ClaudeCodeAvailable(t *testing.T) {
	rl := NewRuntimeLauncher()
	cmd, args, err := rl.GetCommand(model.RuntimeClaudeCode, RuntimeLaunchOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd != "claude" {
		t.Errorf("expected claude, got %s", cmd)
	}
	if len(args) != 0 {
		t.Errorf("expected no args, got %v", args)
	}
}

func TestRuntimeLauncher_RejectsUnsupportedRuntimes(t *testing.T) {
	rl := NewRuntimeLauncher()
	// codex / gemini are now registered (opt-in escape hatch); only unknown
	// runtimes remain unsupported.
	for _, runtime := range []string{"unknown-runtime"} {
		t.Run(runtime, func(t *testing.T) {
			_, _, err := rl.GetCommand(runtime, RuntimeLaunchOptions{})
			if err == nil {
				t.Fatal("expected unsupported runtime error")
			}
		})
	}
}

// TestRuntimeLauncher_CodexBypassFlag pins
// `--dangerously-bypass-approvals-and-sandbox` for codex workers. Removing
// or replacing the flag would re-introduce two distinct E2E breakages:
//
//   - Without sandbox bypass, codex blocks Unix-socket connect(2) to
//     .maestro/daemon.sock, so `maestro result write` fails and the
//     command stalls with the worker stuck in_progress.
//   - Without approval bypass, codex stops on the first-launch
//     "Do you trust the contents of this directory?" prompt and on every
//     tool call, neither of which a maestro worker pane can answer.
//
// This test ensures nobody accidentally re-tightens the flags without
// replacing them with an equivalent that still permits the daemon IPC and
// unattended approvals.
func TestRuntimeLauncher_CodexBypassFlag(t *testing.T) {
	rl := NewRuntimeLauncher()
	cmd, args, err := rl.GetCommand(model.RuntimeCodex, RuntimeLaunchOptions{})
	if err != nil {
		t.Fatalf("GetCommand(codex): %v", err)
	}
	if cmd != "codex" {
		t.Errorf("cmd = %q, want codex", cmd)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--dangerously-bypass-approvals-and-sandbox") {
		t.Errorf("codex args missing --dangerously-bypass-approvals-and-sandbox (would block daemon socket / trust prompt): %v", args)
	}
}

// TestRuntimeLauncher_GeminiYoloFlag verifies gemini auto-approves tool
// calls via `--yolo`. Without it, gemini opens an interactive Allow prompt
// for every WriteFile / Edit / Shell action, which a maestro worker pane
// cannot respond to.
func TestRuntimeLauncher_GeminiYoloFlag(t *testing.T) {
	rl := NewRuntimeLauncher()
	cmd, args, err := rl.GetCommand(model.RuntimeGemini, RuntimeLaunchOptions{})
	if err != nil {
		t.Fatalf("GetCommand(gemini): %v", err)
	}
	if cmd != "gemini" {
		t.Errorf("cmd = %q, want gemini", cmd)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--yolo") {
		t.Errorf("gemini args missing --yolo (would block on tool confirmations): %v", args)
	}
}

// TestRuntimeLauncher_CodexPropagatesModelAndPrompt pins down model and
// prompt wiring for codex workers. Both fields used to be silently dropped
// because buildRuntimeArgs only handled the claude-code branch, which left
// Codex workers running on codex's default model (regardless of the
// agents.workers.model configured in maestro) and with no role guidance
// at all (the worker.md system prompt was assembled but never reached the
// runtime).
func TestRuntimeLauncher_CodexPropagatesModelAndPrompt(t *testing.T) {
	rl := NewRuntimeLauncher()
	_, args, err := rl.GetCommand(model.RuntimeCodex, RuntimeLaunchOptions{
		Model:  "gpt-5.2-codex",
		Prompt: "you are a maestro worker; acknowledge with READY",
	})
	if err != nil {
		t.Fatalf("GetCommand(codex): %v", err)
	}
	if !sliceContainsPair(args, "--model", "gpt-5.2-codex") {
		t.Errorf("codex args missing --model gpt-5.2-codex (would silently fall back to codex default model): %v", args)
	}
	if !sliceContains(args, "you are a maestro worker; acknowledge with READY") {
		t.Errorf("codex args missing the system prompt as the positional [PROMPT] seed (worker.md role guidance would be lost): %v", args)
	}
}

// TestRuntimeLauncher_GeminiPropagatesModelAndPrompt mirrors the codex test
// for gemini. ParseRuntimeFromModel routes `gemini-*` strings into the
// gemini runtime with the explicit model name preserved, so dropping
// --model here would leave operator-configured `gemini-2.5-pro` requests
// running on whatever model the gemini CLI defaulted to.
func TestRuntimeLauncher_GeminiPropagatesModelAndPrompt(t *testing.T) {
	rl := NewRuntimeLauncher()
	_, args, err := rl.GetCommand(model.RuntimeGemini, RuntimeLaunchOptions{
		Model:  "gemini-2.5-pro",
		Prompt: "system prompt body",
	})
	if err != nil {
		t.Fatalf("GetCommand(gemini): %v", err)
	}
	if !sliceContainsPair(args, "--model", "gemini-2.5-pro") {
		t.Errorf("gemini args missing --model gemini-2.5-pro (operator-configured model is dropped): %v", args)
	}
	// gemini uses --prompt-interactive so the REPL stays alive after seeding
	// (--prompt / -p is the headless mode and would exit before the
	// dispatcher could deliver the first task envelope).
	if !sliceContainsPair(args, "--prompt-interactive", "system prompt body") {
		t.Errorf("gemini args missing --prompt-interactive seed (system prompt would be silently dropped): %v", args)
	}
}

func sliceContains(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func sliceContainsPair(args []string, flag, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func TestRuntimeLauncher_EmptyRuntimeDefaultsToClaudeCode(t *testing.T) {
	rl := NewRuntimeLauncher()
	cmd, _, err := rl.GetCommand("", RuntimeLaunchOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd != "claude" {
		t.Errorf("expected claude, got %s", cmd)
	}
}

func TestRuntimeLauncher_ClaudeCodeWithModel(t *testing.T) {
	rl := NewRuntimeLauncher()
	_, args, err := rl.GetCommand(model.RuntimeClaudeCode, RuntimeLaunchOptions{
		Model:  "opus",
		Prompt: "ignored prompt",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := []string{"--model", "opus"}
	if len(args) != len(expected) {
		t.Fatalf("expected args %v, got %v", expected, args)
	}
	for i, e := range expected {
		if args[i] != e {
			t.Errorf("args[%d]: expected %q, got %q", i, e, args[i])
		}
	}
}

func TestFallbackToDefault_AlwaysClaudeCode(t *testing.T) {
	rl := NewRuntimeLauncher()
	cmd, args := rl.FallbackToDefault()
	if cmd != "claude" {
		t.Errorf("expected claude, got %s", cmd)
	}
	if len(args) != 0 {
		t.Errorf("expected no default args, got %v", args)
	}
}

// TestLaunchAlternativeRuntimeRejectsOrchestratorAndPlanner pins the
// orchestrator / planner restriction: codex / gemini must never be allowed
// for those roles because they operate directly on the project root with
// maestro-CLI access, and the validate_run_on_main pre-flight is not
// enough to bound their blast radius.
func TestLaunchAlternativeRuntimeRejectsOrchestratorAndPlanner(t *testing.T) {
	tests := []struct {
		role    string
		runtime string
	}{
		{"orchestrator", model.RuntimeCodex},
		{"orchestrator", model.RuntimeGemini},
		{"planner", model.RuntimeCodex},
		{"planner", model.RuntimeGemini},
	}
	for _, tc := range tests {
		t.Run(tc.role+"_"+tc.runtime, func(t *testing.T) {
			err := launchAlternativeRuntime(tc.runtime, "", tc.role, "ignored prompt")
			if err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

// TestPrepareCodexHomeForCurrentWorker_WritesTrustEntries pins the
// CODEX_HOME-based trust workaround. Earlier `-c projects.<path>.trust_level
// =trusted` overrides were inconsistently honoured by codex 0.125.0's CLI
// parser when the path contained `/`, so worker panes still landed on the
// first-run "Do you trust the contents of this directory?" modal and
// silently consumed the dispatcher's paste+Enter (the 2026-04 audit
// regression). The fix is to stage a per-worker CODEX_HOME containing a
// real config.toml that lists the trust entry — codex's TOML loader
// honours this path consistently.
func TestPrepareCodexHomeForCurrentWorker_WritesTrustEntries(t *testing.T) {
	// Build a fake user CODEX_HOME with one auxiliary file and a config
	// that should survive into the per-worker copy.
	userHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(userHome, "config.toml"),
		[]byte("model = \"o3\"\n"), 0o600); err != nil {
		t.Fatalf("seed user config.toml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(userHome, "auth.json"),
		[]byte("{\"token\":\"x\"}"), 0o600); err != nil {
		t.Fatalf("seed user auth.json: %v", err)
	}
	t.Setenv("CODEX_HOME", userHome)
	// Run the worker from a known cwd so the trust entry is predictable.
	workerDir := t.TempDir()
	prevCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(workerDir); err != nil {
		t.Fatalf("chdir worker: %v", err)
	}
	defer func() { _ = os.Chdir(prevCwd) }()

	codexHome, err := prepareCodexHomeForCurrentWorker()
	if err != nil {
		t.Fatalf("prepareCodexHomeForCurrentWorker: %v", err)
	}
	if codexHome == "" {
		t.Fatal("expected non-empty CODEX_HOME path when user codex dir exists")
	}
	t.Cleanup(func() { _ = os.RemoveAll(codexHome) })

	// auth.json must be reachable through the new CODEX_HOME so codex can
	// authenticate; we mirror it as a symlink.
	if _, err := os.Stat(filepath.Join(codexHome, "auth.json")); err != nil {
		t.Errorf("auth.json must be mirrored into per-worker CODEX_HOME: %v", err)
	}

	cfg, err := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	if err != nil {
		t.Fatalf("read per-worker config: %v", err)
	}
	cfgStr := string(cfg)
	if !strings.Contains(cfgStr, "model = \"o3\"") {
		t.Errorf("user's original config must be preserved at the top, got: %s", cfgStr)
	}
	if !strings.Contains(cfgStr, "trust_level = \"trusted\"") {
		t.Errorf("trust override missing from per-worker config: %s", cfgStr)
	}
	// EvalSymlinks may resolve workerDir to a different path on macOS
	// (/var/folders/... ↔ /private/var/folders/...). Both paths must be
	// trusted so codex matches whichever form it canonicalises.
	resolved, evalErr := filepath.EvalSymlinks(workerDir)
	if evalErr == nil && resolved != workerDir {
		if !strings.Contains(cfgStr, resolved) {
			t.Errorf("symlink-resolved worker path %q must also be trusted, got: %s", resolved, cfgStr)
		}
	}
	if !strings.Contains(cfgStr, workerDir) && (evalErr != nil || !strings.Contains(cfgStr, resolved)) {
		t.Errorf("at least one form of the worker path must be trusted, got: %s", cfgStr)
	}
}

// TestPrepareCodexHomeForCurrentWorker_StripsExistingProjectSection guards
// against TOML duplicate-table errors when the user's config.toml already
// contains a `[projects."<workerCwd>"]` section. The previous revision
// blindly appended a second `[projects."<path>"]` header, producing TOML
// that codex's loader rejects with a duplicate-table error. The fix strips
// any pre-existing section for the same path before appending our trust
// entry, so codex can parse the per-worker config and the worker pane
// proceeds past the trust prompt.
func TestPrepareCodexHomeForCurrentWorker_StripsExistingProjectSection(t *testing.T) {
	userHome := t.TempDir()
	workerDir := t.TempDir()
	// Pre-seed the user's config with a competing trust entry for the
	// exact same path the worker will run from. Without the strip, the
	// per-worker config.toml ends up with two `[projects."<workerDir>"]`
	// headers — invalid TOML.
	original := "model = \"o3\"\n" +
		fmt.Sprintf("[projects.%q]\ntrust_level = %q\n", workerDir, "untrusted")
	if err := os.WriteFile(filepath.Join(userHome, "config.toml"),
		[]byte(original), 0o600); err != nil {
		t.Fatalf("seed user config.toml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(userHome, "auth.json"),
		[]byte("{\"token\":\"x\"}"), 0o600); err != nil {
		t.Fatalf("seed user auth.json: %v", err)
	}
	t.Setenv("CODEX_HOME", userHome)
	prevCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(workerDir); err != nil {
		t.Fatalf("chdir worker: %v", err)
	}
	defer func() { _ = os.Chdir(prevCwd) }()

	codexHome, err := prepareCodexHomeForCurrentWorker()
	if err != nil {
		t.Fatalf("prepareCodexHomeForCurrentWorker: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(codexHome) })

	cfg, err := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	if err != nil {
		t.Fatalf("read per-worker config: %v", err)
	}
	cfgStr := string(cfg)

	// Unrelated user setting must survive (model = "o3" was outside any
	// projects table).
	if !strings.Contains(cfgStr, "model = \"o3\"") {
		t.Errorf("unrelated user config setting was lost: %s", cfgStr)
	}
	// The user's untrusted entry must NOT appear — otherwise we'd have a
	// duplicate `[projects."<path>"]` table or the wrong trust_level.
	if strings.Contains(cfgStr, "untrusted") {
		t.Errorf("user's untrusted entry must be stripped before our trusted append: %s", cfgStr)
	}
	// Duplicate-table guard: every `[projects.*]` header in the produced
	// file must be unique. TOML treats two identical standard-table
	// headers as a parse error and codex would refuse to load the config.
	headerCounts := make(map[string]int)
	for _, line := range strings.Split(cfgStr, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "[projects.") {
			continue
		}
		if strings.HasPrefix(trimmed, "[[") {
			continue
		}
		headerCounts[trimmed]++
	}
	if len(headerCounts) == 0 {
		t.Errorf("no [projects.*] header survived; trust override is missing: %s", cfgStr)
	}
	for h, count := range headerCounts {
		if count != 1 {
			t.Errorf("header %s appears %d times in per-worker config (TOML duplicate-table — codex refuses to load): %s", h, count, cfgStr)
		}
	}
	if !strings.Contains(cfgStr, "trust_level = \"trusted\"") {
		t.Errorf("trust override missing from per-worker config after strip: %s", cfgStr)
	}
}

// TestPrepareCodexHomeForCurrentWorker_NoUserHomeIsNoOp verifies the
// helper returns "" (signalling "skip the override, fall through to
// codex's own defaults") when the user has no ~/.codex directory. We
// deliberately do not synthesise an empty CODEX_HOME because that would
// mask a fresh codex install where the first-run trust prompt is the
// expected and correct UX.
func TestPrepareCodexHomeForCurrentWorker_NoUserHomeIsNoOp(t *testing.T) {
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "missing"))
	got, err := prepareCodexHomeForCurrentWorker()
	if err != nil {
		t.Fatalf("expected nil error when user codex home is absent, got: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty path, got: %q", got)
	}
}

// TestGCStaleCodexHomes pins the daemon-startup GC contract: a tempdir
// belonging to a dead PID is removed; one belonging to a live PID is
// preserved; entries that do not match the maestro-codex-* pattern are
// untouched. The lookup uses signal-0 against the OS, so we use the
// current test process's PID (always alive) and PID 1 as the live
// reference, plus a non-existent high PID as the dead case.
func TestGCStaleCodexHomes(t *testing.T) {
	// Use a private tmp root so we do not touch the host's TempDir.
	tmpRoot := t.TempDir()
	t.Setenv("TMPDIR", tmpRoot) // os.TempDir reads $TMPDIR on macOS/Linux

	// Find a dead PID. /proc enumeration would be platform-specific;
	// instead use a value high enough to be empirically unused across
	// macOS/Linux. If the host happens to have this PID, the test will
	// over-conservatively skip the dir — never wrong-direction.
	const probablyDeadPID = 999999

	livePID := os.Getpid()
	dirs := []struct {
		name     string
		expected string // "removed" | "kept"
	}{
		{fmt.Sprintf("maestro-codex-%d-worker1", probablyDeadPID), "removed"},
		{fmt.Sprintf("maestro-codex-%d-worker2", livePID), "kept"},
		{"maestro-codex-notanint-worker3", "kept"},                             // malformed → leave alone
		{"maestro-codex-", "kept"},                                             // truncated prefix → leave alone
		{fmt.Sprintf("not-codex-%d-worker4", probablyDeadPID), "kept"},         // unrelated dir
		{fmt.Sprintf("maestro-codex-%d-worker_x", probablyDeadPID), "removed"}, // underscore in agent_id is allowed
	}
	for _, d := range dirs {
		path := filepath.Join(tmpRoot, d.name)
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
	}

	removed, errs := GCStaleCodexHomes()
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2 (only the two dead-PID maestro-codex dirs)", removed)
	}

	for _, d := range dirs {
		path := filepath.Join(tmpRoot, d.name)
		_, err := os.Stat(path)
		switch d.expected {
		case "removed":
			if !os.IsNotExist(err) {
				t.Errorf("%s should be removed, but Stat returned: err=%v", d.name, err)
			}
		case "kept":
			if err != nil {
				t.Errorf("%s should remain, but Stat returned: %v", d.name, err)
			}
		}
	}
}

// TestBuildAlternativeWorkerEnv_NonCodexPassesThrough guards against the
// CODEX_HOME plumbing leaking onto gemini workers (where it would point
// gemini at a config dir codex generated and cause confused-deputy bugs).
func TestBuildAlternativeWorkerEnv_NonCodexPassesThrough(t *testing.T) {
	t.Setenv("CODEX_HOME", "/should/not/leak")
	env, err := buildAlternativeWorkerEnv(model.RuntimeGemini)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "CODEX_HOME=") && kv != "CODEX_HOME=/should/not/leak" {
			t.Errorf("non-codex env must inherit CODEX_HOME unchanged, got: %s", kv)
		}
	}
}
