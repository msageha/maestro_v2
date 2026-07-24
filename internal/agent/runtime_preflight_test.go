package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

// newFakePreflight returns a RuntimePreflight whose collaborators are all
// inert fakes: nothing touches the host PATH, env, home directory, or forks
// a process. Individual tests override the fields they exercise. This keeps
// the suite runnable inside sandboxes that deny external process spawns.
func newFakePreflight() *RuntimePreflight {
	return &RuntimePreflight{
		LookPath: func(file string) (string, error) { return "/fake/bin/" + file, nil },
		RunCommand: func(_ context.Context, _ int, name string, _ ...string) ([]byte, error) {
			return []byte(name + " 1.0.0\n"), nil
		},
		Getenv:         func(string) string { return "" },
		UserHomeDir:    func() (string, error) { return "", errors.New("no home in fake") },
		Timeout:        DefaultRuntimeProbeTimeout,
		MaxOutputBytes: DefaultRuntimeProbeMaxOutputBytes,
	}
}

func TestConfiguredRuntimes_MixedWorkers(t *testing.T) {
	t.Parallel()
	cfg := model.Config{}
	cfg.Agents.Orchestrator.Model = "opus"
	cfg.Agents.Planner.Model = "sonnet"
	cfg.Agents.Workers.Count = 3
	cfg.Agents.Workers.DefaultModel = "sonnet"
	cfg.Agents.Workers.Models = map[string]string{
		"worker2": "codex",
		"worker3": "gemini-2.5-pro",
	}

	got := ConfiguredRuntimes(cfg)
	want := []ConfiguredRuntime{
		{Name: model.RuntimeClaudeCode, Agents: []string{"orchestrator", "planner", "worker1"}},
		{Name: model.RuntimeCodex, Agents: []string{"worker2"}},
		{Name: model.RuntimeGemini, Agents: []string{"worker3"}},
	}
	assertConfiguredRuntimes(t, got, want)
}

// TestConfiguredRuntimes_BoostForcesClaude pins the boost interaction: boost
// promotes every worker to opus (model.ResolveWorkerModel), so codex/gemini
// entries in the per-worker Models map must NOT surface as configured
// runtimes — preflight would otherwise fail a boosted launch over a binary
// the formation never spawns.
func TestConfiguredRuntimes_BoostForcesClaude(t *testing.T) {
	t.Parallel()
	cfg := model.Config{}
	cfg.Agents.Workers.Count = 2
	cfg.Agents.Workers.Boost = true
	cfg.Agents.Workers.Models = map[string]string{"worker1": "codex", "worker2": "gemini"}

	got := ConfiguredRuntimes(cfg)
	want := []ConfiguredRuntime{
		{Name: model.RuntimeClaudeCode, Agents: []string{"orchestrator", "planner", "worker1", "worker2"}},
	}
	assertConfiguredRuntimes(t, got, want)
}

// TestConfiguredRuntimes_ZeroWorkersStillHasOne mirrors formation's
// max(Count, 1): a zero worker count still launches worker1.
func TestConfiguredRuntimes_ZeroWorkersStillHasOne(t *testing.T) {
	t.Parallel()
	cfg := model.Config{}
	got := ConfiguredRuntimes(cfg)
	want := []ConfiguredRuntime{
		{Name: model.RuntimeClaudeCode, Agents: []string{"orchestrator", "planner", "worker1"}},
	}
	assertConfiguredRuntimes(t, got, want)
}

func assertConfiguredRuntimes(t *testing.T, got, want []ConfiguredRuntime) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("ConfiguredRuntimes = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Name != want[i].Name {
			t.Errorf("runtime[%d].Name = %q, want %q", i, got[i].Name, want[i].Name)
		}
		if strings.Join(got[i].Agents, ",") != strings.Join(want[i].Agents, ",") {
			t.Errorf("runtime[%d].Agents = %v, want %v", i, got[i].Agents, want[i].Agents)
		}
	}
}

func TestRuntimeCommandName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		runtime string
		want    string
	}{
		{model.RuntimeClaudeCode, "claude"},
		{model.RuntimeCodex, "codex"},
		{model.RuntimeGemini, "gemini"},
		{"", "claude"}, // empty falls back to the default runtime
	}
	for _, tc := range cases {
		got, err := RuntimeCommandName(tc.runtime)
		if err != nil {
			t.Errorf("RuntimeCommandName(%q): %v", tc.runtime, err)
			continue
		}
		if got != tc.want {
			t.Errorf("RuntimeCommandName(%q) = %q, want %q", tc.runtime, got, tc.want)
		}
	}
	if _, err := RuntimeCommandName("unknown-runtime"); err == nil {
		t.Error("RuntimeCommandName(unknown-runtime) must error")
	}
}

func TestCheckBinary_MissingBinary(t *testing.T) {
	t.Parallel()
	p := newFakePreflight()
	p.LookPath = func(file string) (string, error) {
		return "", fmt.Errorf("exec: %q: executable file not found in $PATH", file)
	}
	cmdName, path, err := p.CheckBinary(model.RuntimeCodex)
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
	if cmdName != "codex" {
		t.Errorf("cmdName = %q, want codex (needed for the diagnostic even on failure)", cmdName)
	}
	if path != "" {
		t.Errorf("path = %q, want empty on failure", path)
	}
}

func TestCheckAuth_EnvVarWins(t *testing.T) {
	t.Parallel()
	cases := []struct {
		runtime string
		envKey  string
	}{
		{model.RuntimeClaudeCode, "ANTHROPIC_API_KEY"},
		{model.RuntimeClaudeCode, "CLAUDE_CODE_OAUTH_TOKEN"},
		{model.RuntimeCodex, "OPENAI_API_KEY"},
		{model.RuntimeGemini, "GEMINI_API_KEY"},
		{model.RuntimeGemini, "GOOGLE_API_KEY"},
	}
	for _, tc := range cases {
		t.Run(tc.runtime+"/"+tc.envKey, func(t *testing.T) {
			t.Parallel()
			p := newFakePreflight()
			p.Getenv = func(key string) string {
				if key == tc.envKey {
					return "secret-value"
				}
				return ""
			}
			status, detail := p.CheckAuth(tc.runtime)
			if status != RuntimeAuthOK {
				t.Fatalf("status = %q, want ok (detail: %s)", status, detail)
			}
			if !strings.Contains(detail, tc.envKey) {
				t.Errorf("detail should name the env var %s, got: %s", tc.envKey, detail)
			}
			if strings.Contains(detail, "secret-value") {
				t.Errorf("detail must never include the credential value: %s", detail)
			}
		})
	}
}

func TestCheckAuth_CredentialFiles(t *testing.T) {
	t.Parallel()
	cases := []struct {
		runtime string
		relPath []string
	}{
		{model.RuntimeClaudeCode, []string{".claude", ".credentials.json"}},
		{model.RuntimeCodex, []string{".codex", "auth.json"}},
		{model.RuntimeGemini, []string{".gemini", "oauth_creds.json"}},
	}
	for _, tc := range cases {
		t.Run(tc.runtime, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			credPath := filepath.Join(append([]string{home}, tc.relPath...)...)
			if err := os.MkdirAll(filepath.Dir(credPath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(credPath, []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}

			p := newFakePreflight()
			p.UserHomeDir = func() (string, error) { return home, nil }
			status, detail := p.CheckAuth(tc.runtime)
			if status != RuntimeAuthOK {
				t.Fatalf("status = %q, want ok (detail: %s)", status, detail)
			}
			if !strings.Contains(detail, credPath) {
				t.Errorf("detail should name the credential path %s, got: %s", credPath, detail)
			}
		})
	}
}

// TestCheckAuth_CodexHomeOverride pins the CODEX_HOME env override: when the
// operator relocated their codex home, auth.json is looked up there, not in
// ~/.codex (matching defaultUserCodexHome in launcher.go).
func TestCheckAuth_CodexHomeOverride(t *testing.T) {
	t.Parallel()
	codexHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := newFakePreflight()
	p.Getenv = func(key string) string {
		if key == "CODEX_HOME" {
			return codexHome
		}
		return ""
	}
	status, detail := p.CheckAuth(model.RuntimeCodex)
	if status != RuntimeAuthOK {
		t.Fatalf("status = %q, want ok (detail: %s)", status, detail)
	}
	if !strings.Contains(detail, codexHome) {
		t.Errorf("detail should point at the CODEX_HOME path, got: %s", detail)
	}
}

// TestCheckAuth_UndetectableIsUnknownNotFail pins the "warning, never a hard
// stop" contract: with no env vars and no credential files, every runtime
// reports unknown with a remediation hint. There is no fail path for auth.
func TestCheckAuth_UndetectableIsUnknownNotFail(t *testing.T) {
	t.Parallel()
	for _, runtime := range []string{model.RuntimeClaudeCode, model.RuntimeCodex, model.RuntimeGemini} {
		t.Run(runtime, func(t *testing.T) {
			t.Parallel()
			p := newFakePreflight()
			p.UserHomeDir = func() (string, error) { return t.TempDir(), nil }
			status, detail := p.CheckAuth(runtime)
			if status != RuntimeAuthUnknown {
				t.Fatalf("status = %q, want unknown", status)
			}
			if detail == "" {
				t.Error("unknown auth must carry a remediation hint")
			}
		})
	}
}

func TestProbeVersion_FirstLineTrimmed(t *testing.T) {
	t.Parallel()
	p := newFakePreflight()
	p.RunCommand = func(_ context.Context, _ int, _ string, _ ...string) ([]byte, error) {
		return []byte("\n  2.1.190 (Claude Code)  \nextra noise\n"), nil
	}
	got, err := p.ProbeVersion(context.Background(), model.RuntimeClaudeCode, "claude")
	if err != nil {
		t.Fatalf("ProbeVersion: %v", err)
	}
	if got != "2.1.190 (Claude Code)" {
		t.Errorf("version = %q, want first non-empty trimmed line", got)
	}
}

// TestProbeVersion_SkipsWarningPreamble pins the versionLine heuristic:
// codex prepends startup warnings (e.g. a sandbox PATH-alias warning) to
// its --version output, and reporting the warning as the version would
// mislead operators. The first line containing a dotted version number
// wins; when none matches, the first non-empty line is the fallback.
func TestProbeVersion_SkipsWarningPreamble(t *testing.T) {
	t.Parallel()
	p := newFakePreflight()
	p.RunCommand = func(_ context.Context, _ int, _ string, _ ...string) ([]byte, error) {
		return []byte("WARNING: proceeding, even though we could not create PATH aliases\ncodex-cli 0.125.0\n"), nil
	}
	got, err := p.ProbeVersion(context.Background(), model.RuntimeCodex, "codex")
	if err != nil {
		t.Fatalf("ProbeVersion: %v", err)
	}
	if got != "codex-cli 0.125.0" {
		t.Errorf("version = %q, want the version-number line, not the warning preamble", got)
	}

	p.RunCommand = func(_ context.Context, _ int, _ string, _ ...string) ([]byte, error) {
		return []byte("no numbers here\nsecond line\n"), nil
	}
	got, err = p.ProbeVersion(context.Background(), model.RuntimeCodex, "codex")
	if err != nil {
		t.Fatalf("ProbeVersion fallback: %v", err)
	}
	if got != "no numbers here" {
		t.Errorf("fallback = %q, want first non-empty line", got)
	}
}

func TestProbeVersion_TimeoutClassified(t *testing.T) {
	t.Parallel()
	p := newFakePreflight()
	p.Timeout = 10 * time.Millisecond
	p.RunCommand = func(ctx context.Context, _ int, _ string, _ ...string) ([]byte, error) {
		<-ctx.Done() // simulate a probe that hangs until killed
		return nil, ctx.Err()
	}
	_, err := p.ProbeVersion(context.Background(), model.RuntimeCodex, "codex")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("timeout should be classified explicitly, got: %v", err)
	}
}

func TestProbeVersion_UnknownRuntimeNotProbedBlind(t *testing.T) {
	t.Parallel()
	p := newFakePreflight()
	called := false
	p.RunCommand = func(_ context.Context, _ int, _ string, _ ...string) ([]byte, error) {
		called = true
		return []byte("x"), nil
	}
	if _, err := p.ProbeVersion(context.Background(), "unknown-runtime", "mystery"); err == nil {
		t.Fatal("expected error for runtime without a version-probe definition")
	}
	if called {
		t.Error("a runtime without a probe definition must not be executed blind")
	}
}

func TestCheck_MissingBinaryFailsAndSkipsProbes(t *testing.T) {
	t.Parallel()
	p := newFakePreflight()
	p.LookPath = func(string) (string, error) { return "", errors.New("not found") }
	probed := false
	p.RunCommand = func(_ context.Context, _ int, _ string, _ ...string) ([]byte, error) {
		probed = true
		return nil, nil
	}

	res := p.Check(context.Background(), model.RuntimeGemini, []string{"worker2"})
	if res.Status != RuntimeCheckFail {
		t.Errorf("Status = %q, want fail", res.Status)
	}
	if res.BinaryError == "" {
		t.Error("BinaryError must carry the resolution failure")
	}
	if probed {
		t.Error("version probe must be skipped when the binary is missing")
	}
	if res.Command != "gemini" {
		t.Errorf("Command = %q, want gemini", res.Command)
	}
}

func TestCheck_VersionProbeFailureIsWarnNotFail(t *testing.T) {
	t.Parallel()
	p := newFakePreflight()
	p.Getenv = func(key string) string {
		if key == "OPENAI_API_KEY" {
			return "x"
		}
		return ""
	}
	p.RunCommand = func(_ context.Context, _ int, _ string, _ ...string) ([]byte, error) {
		return []byte("boom"), errors.New("exit status 1")
	}

	res := p.Check(context.Background(), model.RuntimeCodex, []string{"worker1"})
	if res.Status != RuntimeCheckWarn {
		t.Errorf("Status = %q, want warn (version probe failures must not hard-fail)", res.Status)
	}
	if res.VersionError == "" {
		t.Error("VersionError must be populated")
	}
	if res.AuthStatus != RuntimeAuthOK {
		t.Errorf("AuthStatus = %q, want ok", res.AuthStatus)
	}
}

func TestCheck_AllGreen(t *testing.T) {
	t.Parallel()
	p := newFakePreflight()
	p.Getenv = func(key string) string {
		if key == "ANTHROPIC_API_KEY" {
			return "x"
		}
		return ""
	}
	res := p.Check(context.Background(), model.RuntimeClaudeCode, []string{"orchestrator"})
	if res.Status != RuntimeCheckOK {
		t.Fatalf("Status = %q, want ok (%+v)", res.Status, res)
	}
	if res.BinaryPath != "/fake/bin/claude" {
		t.Errorf("BinaryPath = %q", res.BinaryPath)
	}
	if res.Version != "claude 1.0.0" {
		t.Errorf("Version = %q", res.Version)
	}
}

func TestCheck_AuthUnknownIsWarn(t *testing.T) {
	t.Parallel()
	p := newFakePreflight()
	res := p.Check(context.Background(), model.RuntimeClaudeCode, nil)
	if res.Status != RuntimeCheckWarn {
		t.Errorf("Status = %q, want warn for unknown auth", res.Status)
	}
	if res.AuthStatus != RuntimeAuthUnknown {
		t.Errorf("AuthStatus = %q, want unknown", res.AuthStatus)
	}
}

func TestCappedWriter_TruncatesButReportsFullWrite(t *testing.T) {
	t.Parallel()
	w := &cappedWriter{max: 8}
	n, err := w.Write([]byte("0123456789"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 10 {
		t.Errorf("n = %d, want 10 (short writes would break the child's stdout copy)", n)
	}
	n, err = w.Write([]byte("overflow"))
	if err != nil || n != 8 {
		t.Errorf("second Write = (%d, %v), want (8, nil)", n, err)
	}
	if got := string(w.Bytes()); got != "01234567" {
		t.Errorf("retained = %q, want first 8 bytes only", got)
	}
}
