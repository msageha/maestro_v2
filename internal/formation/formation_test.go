package formation

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

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

func TestDefaultConfig_NonZeroValues(t *testing.T) {
	t.Parallel()

	// Verify defaultConfig fields return consistent, non-zero values.
	if got := defaultConfig.DaemonPollTimeout; got <= 0 {
		t.Errorf("DaemonPollTimeout = %v, want > 0", got)
	}
	if got := defaultConfig.DaemonPollInterval; got <= 0 {
		t.Errorf("DaemonPollInterval = %v, want > 0", got)
	}
	if got := defaultConfig.ProcessExitPollInterval; got <= 0 {
		t.Errorf("ProcessExitPollInterval = %v, want > 0", got)
	}
	if got := defaultConfig.PostSignalWait; got <= 0 {
		t.Errorf("PostSignalWait = %v, want > 0", got)
	}
	if got := defaultConfig.WaitReadyPollInterval; got <= 0 {
		t.Errorf("WaitReadyPollInterval = %v, want > 0", got)
	}

	if defaultConfig.ProcMgr == nil {
		t.Error("ProcMgr should not be nil")
	}
}
