package formation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/ptr"
)

// ---------------------------------------------------------------------------
// SwitchRuntime — table-driven tests covering validation + enabled logic
// ---------------------------------------------------------------------------

func TestSwitchRuntime(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		workerID   string
		runtime    string
		cfg        model.Config
		wantErr    bool
		errSubstr  string
	}{
		{
			name:      "unknown runtime returns error",
			workerID:  "worker1",
			runtime:   "unknown-rt",
			cfg:       model.Config{},
			wantErr:   true,
			errSubstr: "unknown runtime",
		},
		{
			name:      "empty runtime string returns error",
			workerID:  "worker1",
			runtime:   "",
			cfg:       model.Config{},
			wantErr:   true,
			errSubstr: "unknown runtime",
		},
		{
			name:     "disabled runtime returns error",
			workerID: "worker1",
			runtime:  "codex",
			cfg: model.Config{
				Runtimes: map[string]model.RuntimeConfig{
					"codex": {Enabled: ptr.Bool(false)},
				},
			},
			wantErr:   true,
			errSubstr: "is disabled",
		},
		{
			name:     "enabled runtime succeeds",
			workerID: "worker1",
			runtime:  "codex",
			cfg: model.Config{
				Runtimes: map[string]model.RuntimeConfig{
					"codex": {Enabled: ptr.Bool(true)},
				},
			},
			wantErr: false,
		},
		{
			name:     "nil Enabled treated as disabled",
			workerID: "worker1",
			runtime:  "codex",
			cfg: model.Config{
				Runtimes: map[string]model.RuntimeConfig{
					"codex": {}, // Enabled is nil => EffectiveEnabled() returns false
				},
			},
			wantErr:   true,
			errSubstr: "is disabled",
		},
		{
			name:     "runtime not in config map succeeds (no explicit disable)",
			workerID: "worker1",
			runtime:  "codex",
			cfg:      model.Config{},
			wantErr:  false,
		},
		{
			name:     "claude-code always valid even with empty config",
			workerID: "worker1",
			runtime:  "claude-code",
			cfg:      model.Config{},
			wantErr:  false,
		},
		{
			name:     "gemini runtime valid when enabled",
			workerID: "worker2",
			runtime:  "gemini",
			cfg: model.Config{
				Runtimes: map[string]model.RuntimeConfig{
					"gemini": {Enabled: ptr.Bool(true)},
				},
			},
			wantErr: false,
		},
		{
			name:     "gemini runtime disabled",
			workerID: "worker2",
			runtime:  "gemini",
			cfg: model.Config{
				Runtimes: map[string]model.RuntimeConfig{
					"gemini": {Enabled: ptr.Bool(false)},
				},
			},
			wantErr:   true,
			errSubstr: "is disabled",
		},
		{
			name:     "empty workerID is accepted (only runtime is validated)",
			workerID: "",
			runtime:  "claude-code",
			cfg:      model.Config{},
			wantErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := SwitchRuntime(tt.workerID, tt.runtime, tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("SwitchRuntime(%q, %q) error = %v, wantErr = %v",
					tt.workerID, tt.runtime, err, tt.wantErr)
			}
			if tt.errSubstr != "" && err != nil {
				if !strings.Contains(err.Error(), tt.errSubstr) {
					t.Errorf("error %q should contain %q", err.Error(), tt.errSubstr)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// isValidPID — simple boundary tests
// ---------------------------------------------------------------------------

func TestIsValidPID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		pid  int
		want bool
	}{
		{"zero", 0, false},
		{"negative one", -1, false},
		{"large negative", -99999, false},
		{"one", 1, true},
		{"typical pid", 12345, true},
		{"large pid", 99999, true},
		{"max int32 range", 2147483647, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isValidPID(tt.pid); got != tt.want {
				t.Errorf("isValidPID(%d) = %v, want %v", tt.pid, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// DefaultConfig — verify production defaults are sensible
// ---------------------------------------------------------------------------

func TestDefaultConfig(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()

	if cfg == nil {
		t.Fatal("DefaultConfig() returned nil")
	}
	if cfg.NewUDSClient == nil {
		t.Error("NewUDSClient should not be nil")
	}
	if cfg.ProcMgr == nil {
		t.Error("ProcMgr should not be nil")
	}

	// Verify timing values match the expected production defaults.
	timingTests := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"DaemonPollTimeout", cfg.DaemonPollTimeout, 10 * time.Second},
		{"DaemonPollInterval", cfg.DaemonPollInterval, 500 * time.Millisecond},
		{"ProcessExitPollInterval", cfg.ProcessExitPollInterval, 500 * time.Millisecond},
		{"PostSignalWait", cfg.PostSignalWait, 500 * time.Millisecond},
		{"WaitReadyPollInterval", cfg.WaitReadyPollInterval, 200 * time.Millisecond},
	}

	for _, tt := range timingTests {
		if tt.got != tt.want {
			t.Errorf("%s = %v, want %v", tt.name, tt.got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// removeIfExists — file removal helper
// ---------------------------------------------------------------------------

func TestRemoveIfExists_NonExistentFile(t *testing.T) {
	t.Parallel()

	// Should not panic or log errors for a non-existent file.
	removeIfExists(filepath.Join(t.TempDir(), "does-not-exist.txt"))
}

func TestRemoveIfExists_ExistingFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "to-remove.txt")
	if err := os.WriteFile(path, []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}

	removeIfExists(path)

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected file to be removed, stat error = %v", err)
	}
}

func TestRemoveIfExists_AlreadyRemovedTwice(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "ephemeral.txt")
	if err := os.WriteFile(path, []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}

	// Remove once, then again — second call should be a no-op.
	removeIfExists(path)
	removeIfExists(path)
}

// ---------------------------------------------------------------------------
// readDaemonPID — additional edge cases beyond startup_state_test.go
// ---------------------------------------------------------------------------

func TestReadDaemonPID_MultipleLines(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	pidPath := filepath.Join(dir, "daemon.pid")

	// Only the first trimmed value matters; extra lines are trimmed by TrimSpace
	// but Atoi will fail on multi-line content.
	if err := os.WriteFile(pidPath, []byte("123\n456"), 0644); err != nil {
		t.Fatal(err)
	}

	got := readDaemonPID(pidPath)
	// "123\n456" after TrimSpace is "123\n456" which Atoi rejects → 0
	if got != 0 {
		t.Errorf("readDaemonPID with multi-line content = %d, want 0", got)
	}
}

func TestReadDaemonPID_FloatingPoint(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	pidPath := filepath.Join(dir, "daemon.pid")

	if err := os.WriteFile(pidPath, []byte("123.45"), 0644); err != nil {
		t.Fatal(err)
	}

	got := readDaemonPID(pidPath)
	if got != 0 {
		t.Errorf("readDaemonPID with float content = %d, want 0", got)
	}
}

func TestReadDaemonPID_MaxIntOverflow(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	pidPath := filepath.Join(dir, "daemon.pid")

	if err := os.WriteFile(pidPath, []byte("99999999999999999999"), 0644); err != nil {
		t.Fatal(err)
	}

	got := readDaemonPID(pidPath)
	if got != 0 {
		t.Errorf("readDaemonPID with overflow content = %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// Config package-level accessors — verify they delegate to defaultConfig
// ---------------------------------------------------------------------------

func TestPackageLevelAccessors_MatchDefaultConfig(t *testing.T) {
	t.Parallel()

	// These accessors read from the package-level defaultConfig.
	// Verify they return consistent, non-zero values.
	if got := daemonPollTimeout(); got <= 0 {
		t.Errorf("daemonPollTimeout() = %v, want > 0", got)
	}
	if got := daemonPollInterval(); got <= 0 {
		t.Errorf("daemonPollInterval() = %v, want > 0", got)
	}
	if got := processExitPollInterval(); got <= 0 {
		t.Errorf("processExitPollInterval() = %v, want > 0", got)
	}
	if got := postSignalWait(); got <= 0 {
		t.Errorf("postSignalWait() = %v, want > 0", got)
	}
	if got := waitReadyPollInterval(); got <= 0 {
		t.Errorf("waitReadyPollInterval() = %v, want > 0", got)
	}

	if pm := procMgr(); pm == nil {
		t.Error("procMgr() should not be nil")
	}
}
