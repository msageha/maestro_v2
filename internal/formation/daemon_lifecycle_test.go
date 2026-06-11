package formation

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/uds"
)

// --- Mock types ---

type mockProcessManager struct {
	alive     func(pid int) bool
	startTime func(pid int) string
	signal    func(pid int, sig syscall.Signal) error
}

func (m *mockProcessManager) Alive(pid int) bool {
	if m.alive != nil {
		return m.alive(pid)
	}
	return false
}

func (m *mockProcessManager) StartTime(pid int) string {
	if m.startTime != nil {
		return m.startTime(pid)
	}
	return ""
}

func (m *mockProcessManager) Signal(pid int, sig syscall.Signal) error {
	if m.signal != nil {
		return m.signal(pid, sig)
	}
	return nil
}

type mockUDSSender struct {
	sendCommand func(command string, params any) (*uds.Response, error)
}

func (m *mockUDSSender) SendCommand(command string, params any) (*uds.Response, error) {
	if m.sendCommand != nil {
		return m.sendCommand(command, params)
	}
	return &uds.Response{Success: true}, nil
}

// --- Test helpers ---

func testConfigWithFastTimings() *Config {
	return &Config{
		NewUDSClient: func(string, time.Duration) udsSender {
			return &mockUDSSender{}
		},
		ProcMgr:                 &mockProcessManager{},
		DaemonPollTimeout:       100 * time.Millisecond,
		DaemonPollInterval:      10 * time.Millisecond,
		ProcessExitPollInterval: 10 * time.Millisecond,
		PostSignalWait:          10 * time.Millisecond,
		WaitReadyPollInterval:   10 * time.Millisecond,
	}
}

func writePIDAndLock(t *testing.T, maestroDir string, pid int) {
	t.Helper()
	pidPath := filepath.Join(maestroDir, "daemon.pid")
	lockPath := filepath.Join(maestroDir, "locks", "daemon.lock")
	if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d", pid)), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", pid)), 0644); err != nil {
		t.Fatal(err)
	}
}

// --- terminateProcess tests ---

func TestTerminateProcess(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		alive       func(pid int) bool
		signal      func(pid int, sig syscall.Signal) error
		sameProcess func(pid int) bool
		termTimeout time.Duration
		wantResult  terminateResult
		wantErr     bool
	}{
		{
			name:        "already dead",
			alive:       func(int) bool { return false },
			sameProcess: func(int) bool { return true },
			termTimeout: 50 * time.Millisecond,
			wantResult:  terminateStopped,
		},
		{
			name: "dies after SIGTERM",
			alive: func() func(int) bool {
				var count int32
				return func(int) bool {
					c := atomic.AddInt32(&count, 1)
					return c <= 2
				}
			}(),
			signal:      func(int, syscall.Signal) error { return nil },
			sameProcess: func(int) bool { return true },
			termTimeout: time.Second,
			wantResult:  terminateStopped,
		},
		{
			name:        "not target before SIGTERM",
			alive:       func(int) bool { return true },
			sameProcess: func(int) bool { return false },
			termTimeout: 50 * time.Millisecond,
			wantResult:  terminateNotTarget,
		},
		{
			name:   "not target before SIGKILL",
			alive:  func(int) bool { return true },
			signal: func(int, syscall.Signal) error { return nil },
			sameProcess: func() func(int) bool {
				var count int32
				return func(int) bool {
					c := atomic.AddInt32(&count, 1)
					return c == 1 // true for SIGTERM, false for SIGKILL
				}
			}(),
			termTimeout: 50 * time.Millisecond,
			wantResult:  terminateNotTarget,
		},
		{
			// A SIGKILL survivor must NOT report terminateStopped: callers
			// delete daemon.pid on terminateStopped, which would orphan a
			// live daemon with no way to find it again.
			name:        "survives SIGKILL",
			alive:       func(int) bool { return true },
			signal:      func(int, syscall.Signal) error { return nil },
			sameProcess: func(int) bool { return true },
			termTimeout: 50 * time.Millisecond,
			wantResult:  terminateSurvived,
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := testConfigWithFastTimings()
			cfg.ProcMgr = &mockProcessManager{
				alive:  tt.alive,
				signal: tt.signal,
			}

			result, err := cfg.terminateProcess(42, tt.sameProcess, tt.termTimeout)
			if result != tt.wantResult {
				t.Errorf("result = %v, want %v", result, tt.wantResult)
			}
			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

func TestTerminateProcess_SignalsCorrectPID(t *testing.T) {
	t.Parallel()
	var signaledPID int
	var signaledSig syscall.Signal
	var count int32
	cfg := testConfigWithFastTimings()
	cfg.ProcMgr = &mockProcessManager{
		alive: func(int) bool {
			c := atomic.AddInt32(&count, 1)
			return c <= 2
		},
		signal: func(pid int, sig syscall.Signal) error {
			signaledPID = pid
			signaledSig = sig
			return nil
		},
	}

	cfg.terminateProcess(12345, func(int) bool { return true }, time.Second)

	if signaledPID != 12345 {
		t.Errorf("signaled PID = %d, want 12345", signaledPID)
	}
	if signaledSig != syscall.SIGTERM {
		t.Errorf("signaled signal = %v, want SIGTERM", signaledSig)
	}
}

func TestTerminateProcess_EscalatesToSIGKILL(t *testing.T) {
	t.Parallel()
	var signals []syscall.Signal
	cfg := testConfigWithFastTimings()
	cfg.ProcMgr = &mockProcessManager{
		alive:  func(int) bool { return true },
		signal: func(_ int, sig syscall.Signal) error { signals = append(signals, sig); return nil },
	}

	cfg.terminateProcess(42, func(int) bool { return true }, 50*time.Millisecond)

	if len(signals) < 2 {
		t.Fatalf("expected at least 2 signals, got %d", len(signals))
	}
	if signals[0] != syscall.SIGTERM {
		t.Errorf("first signal = %v, want SIGTERM", signals[0])
	}
	if signals[len(signals)-1] != syscall.SIGKILL {
		t.Errorf("last signal = %v, want SIGKILL", signals[len(signals)-1])
	}
}

// --- daemonIdentityChecker tests ---

func TestDaemonIdentityChecker(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		pidInFile     int
		originalPID   int
		origStartTime string
		mockStartTime string
		want          bool
	}{
		{
			name:          "matches PID and start time",
			pidInFile:     12345,
			originalPID:   12345,
			origStartTime: "Mon Jan  1 00:00:00 2024",
			mockStartTime: "Mon Jan  1 00:00:00 2024",
			want:          true,
		},
		{
			name:          "PID file changed",
			pidInFile:     99999,
			originalPID:   12345,
			origStartTime: "Mon Jan  1 00:00:00 2024",
			mockStartTime: "Mon Jan  1 00:00:00 2024",
			want:          false,
		},
		{
			name:          "start time changed (PID reuse)",
			pidInFile:     12345,
			originalPID:   12345,
			origStartTime: "Mon Jan  1 00:00:00 2024",
			mockStartTime: "Tue Jan  2 00:00:00 2024",
			want:          false,
		},
		{
			name:          "start time unavailable (process gone)",
			pidInFile:     12345,
			originalPID:   12345,
			origStartTime: "Mon Jan  1 00:00:00 2024",
			mockStartTime: "",
			want:          false,
		},
		{
			name:          "empty original start time skips check",
			pidInFile:     12345,
			originalPID:   12345,
			origStartTime: "",
			mockStartTime: "anything",
			want:          true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			maestroDir := setupTestMaestroDir(t)
			writePIDAndLock(t, maestroDir, tt.pidInFile)
			cfg := testConfigWithFastTimings()
			cfg.ProcMgr = &mockProcessManager{
				startTime: func(int) string { return tt.mockStartTime },
			}

			checker := cfg.daemonIdentityChecker(maestroDir, tt.originalPID, tt.origStartTime)
			got := checker(tt.originalPID)
			if got != tt.want {
				t.Errorf("checker() = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- stopDaemon tests ---

func TestStopDaemon_NoSocketNoPID_NoError(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	cfg := testConfigWithFastTimings()
	if err := cfg.stopDaemon(maestroDir); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestStopDaemon_UDSShutdown_ProcessDies(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)

	socketPath := filepath.Join(maestroDir, uds.DefaultSocketName)
	os.WriteFile(socketPath, []byte{}, 0644)
	writePIDAndLock(t, maestroDir, 12345)

	var aliveCount int32
	cfg := testConfigWithFastTimings()
	cfg.NewUDSClient = func(string, time.Duration) udsSender {
		return &mockUDSSender{sendCommand: func(string, any) (*uds.Response, error) {
			return &uds.Response{Success: true}, nil
		}}
	}
	cfg.ProcMgr = &mockProcessManager{
		alive: func(int) bool {
			c := atomic.AddInt32(&aliveCount, 1)
			return c <= 1
		},
		startTime: func(int) string { return "start" },
	}

	if err := cfg.stopDaemon(maestroDir); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	pidPath := filepath.Join(maestroDir, "daemon.pid")
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Error("expected daemon.pid to be removed")
	}
}

func TestStopDaemon_UDSFails_TerminateAfterPoll(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)

	socketPath := filepath.Join(maestroDir, uds.DefaultSocketName)
	os.WriteFile(socketPath, []byte{}, 0644)
	writePIDAndLock(t, maestroDir, 12345)

	var sigReceived int32
	cfg := testConfigWithFastTimings()
	cfg.NewUDSClient = func(string, time.Duration) udsSender {
		return &mockUDSSender{sendCommand: func(string, any) (*uds.Response, error) {
			return nil, fmt.Errorf("connection refused")
		}}
	}
	cfg.ProcMgr = &mockProcessManager{
		alive: func(int) bool {
			return atomic.LoadInt32(&sigReceived) == 0
		},
		startTime: func(int) string { return "start" },
		signal: func(_ int, sig syscall.Signal) error {
			if sig == syscall.SIGTERM {
				atomic.StoreInt32(&sigReceived, 1)
			}
			return nil
		},
	}

	if err := cfg.stopDaemon(maestroDir); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

func TestStopDaemon_PIDReused_ReturnsNil(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)

	socketPath := filepath.Join(maestroDir, uds.DefaultSocketName)
	os.WriteFile(socketPath, []byte{}, 0644)
	writePIDAndLock(t, maestroDir, 12345)

	var startTimeCallCount int32
	cfg := testConfigWithFastTimings()
	cfg.NewUDSClient = func(string, time.Duration) udsSender {
		return &mockUDSSender{sendCommand: func(string, any) (*uds.Response, error) {
			return nil, fmt.Errorf("connection refused")
		}}
	}
	cfg.ProcMgr = &mockProcessManager{
		alive: func(int) bool { return true },
		startTime: func(int) string {
			c := atomic.AddInt32(&startTimeCallCount, 1)
			if c == 1 {
				return "original-time"
			}
			return "different-time" // PID was reused
		},
		signal: func(int, syscall.Signal) error { return nil },
	}

	err := cfg.stopDaemon(maestroDir)
	if err != nil {
		t.Fatalf("expected no error (PID reuse → terminateNotTarget), got %v", err)
	}
}

func TestStopDaemon_NoPID_LockAvailable(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)

	// Socket exists but no valid PID (no matching lock)
	socketPath := filepath.Join(maestroDir, uds.DefaultSocketName)
	os.WriteFile(socketPath, []byte{}, 0644)

	cfg := testConfigWithFastTimings()
	cfg.NewUDSClient = func(string, time.Duration) udsSender {
		return &mockUDSSender{sendCommand: func(string, any) (*uds.Response, error) {
			return nil, fmt.Errorf("connection refused")
		}}
	}

	if err := cfg.stopDaemon(maestroDir); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Error("expected socket to be removed")
	}
}

func TestStopDaemon_NoPID_NoLockDir(t *testing.T) {
	t.Parallel()
	// Special case: no locks directory means no daemon has ever run
	maestroDir := setupTestMaestroDir(t)

	// Remove the locks directory
	os.RemoveAll(filepath.Join(maestroDir, "locks"))

	socketPath := filepath.Join(maestroDir, uds.DefaultSocketName)
	os.WriteFile(socketPath, []byte{}, 0644)
	pidPath := filepath.Join(maestroDir, "daemon.pid")
	os.WriteFile(pidPath, []byte("invalid"), 0644)

	cfg := testConfigWithFastTimings()
	cfg.NewUDSClient = func(string, time.Duration) udsSender {
		return &mockUDSSender{sendCommand: func(string, any) (*uds.Response, error) {
			return nil, fmt.Errorf("no socket")
		}}
	}

	if err := cfg.stopDaemon(maestroDir); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Error("expected socket to be removed when lock dir missing")
	}
}

func TestStopDaemon_OnlyPIDFile(t *testing.T) {
	t.Parallel()
	// PID file exists but no socket
	maestroDir := setupTestMaestroDir(t)

	pidPath := filepath.Join(maestroDir, "daemon.pid")
	lockPath := filepath.Join(maestroDir, "locks", "daemon.lock")
	os.WriteFile(pidPath, []byte("12345"), 0644)
	os.WriteFile(lockPath, []byte("12345\n"), 0644)

	var aliveCount int32
	cfg := testConfigWithFastTimings()
	cfg.NewUDSClient = func(string, time.Duration) udsSender {
		return &mockUDSSender{sendCommand: func(string, any) (*uds.Response, error) {
			return nil, fmt.Errorf("no socket")
		}}
	}
	cfg.ProcMgr = &mockProcessManager{
		alive: func(int) bool {
			c := atomic.AddInt32(&aliveCount, 1)
			return c <= 1
		},
		startTime: func(int) string { return "start" },
	}

	if err := cfg.stopDaemon(maestroDir); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Error("expected daemon.pid to be removed")
	}
}

// --- waitDaemonReady tests ---

func TestWaitDaemonReady(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		udsFunc   func(string, any) (*uds.Response, error)
		timeout   time.Duration
		wantErr   bool
		errSubstr string
	}{
		{
			name: "immediate success",
			udsFunc: func(string, any) (*uds.Response, error) {
				return &uds.Response{Success: true}, nil
			},
			timeout: time.Second,
		},
		{
			name: "eventual success after retries",
			udsFunc: func() func(string, any) (*uds.Response, error) {
				var count int32
				return func(string, any) (*uds.Response, error) {
					c := atomic.AddInt32(&count, 1)
					if c < 3 {
						return nil, fmt.Errorf("not ready")
					}
					return &uds.Response{Success: true}, nil
				}
			}(),
			timeout: time.Second,
		},
		{
			name: "timeout when never ready",
			udsFunc: func(string, any) (*uds.Response, error) {
				return nil, fmt.Errorf("not ready")
			},
			timeout:   100 * time.Millisecond,
			wantErr:   true,
			errSubstr: "did not respond",
		},
		{
			name: "success=false then success=true",
			udsFunc: func() func(string, any) (*uds.Response, error) {
				var count int32
				return func(string, any) (*uds.Response, error) {
					c := atomic.AddInt32(&count, 1)
					if c < 2 {
						return &uds.Response{Success: false}, nil
					}
					return &uds.Response{Success: true}, nil
				}
			}(),
			timeout: time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := testConfigWithFastTimings()
			cfg.NewUDSClient = func(string, time.Duration) udsSender {
				return &mockUDSSender{sendCommand: tt.udsFunc}
			}

			err := cfg.waitDaemonReady("/tmp/test.sock", tt.timeout)
			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if tt.errSubstr != "" && err != nil && !strings.Contains(err.Error(), tt.errSubstr) {
				t.Errorf("error %q should contain %q", err.Error(), tt.errSubstr)
			}
		})
	}
}

// --- cleanupStalePID tests ---

func TestCleanupStalePID_ProcessAlive_TerminateSucceeds(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	writePIDAndLock(t, maestroDir, 12345)

	var sigReceived int32
	cfg := testConfigWithFastTimings()
	cfg.ProcMgr = &mockProcessManager{
		alive: func(int) bool {
			return atomic.LoadInt32(&sigReceived) == 0
		},
		startTime: func(int) string { return "start" },
		signal: func(_ int, sig syscall.Signal) error {
			if sig == syscall.SIGTERM {
				atomic.StoreInt32(&sigReceived, 1)
			}
			return nil
		},
	}

	cfg.cleanupStalePID(maestroDir)

	pidPath := filepath.Join(maestroDir, "daemon.pid")
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Error("expected daemon.pid to be removed after cleanup")
	}
}

func TestCleanupStalePID_ProcessDead(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	writePIDAndLock(t, maestroDir, 12345)

	cfg := testConfigWithFastTimings()
	cfg.ProcMgr = &mockProcessManager{
		alive: func(int) bool { return false },
	}

	cfg.cleanupStalePID(maestroDir)

	pidPath := filepath.Join(maestroDir, "daemon.pid")
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Error("expected daemon.pid to be removed for dead process")
	}
}

func TestCleanupStalePID_NoPIDFile(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)

	cfg := testConfigWithFastTimings()
	// No crash expected
	cfg.cleanupStalePID(maestroDir)
}

// --- stopDaemon lock timeout tests ---

func TestStopDaemon_NoPID_LockHeld_Timeout(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)

	socketPath := filepath.Join(maestroDir, uds.DefaultSocketName)
	os.WriteFile(socketPath, []byte{}, 0644)

	cfg := testConfigWithFastTimings()
	cfg.NewUDSClient = func(string, time.Duration) udsSender {
		return &mockUDSSender{sendCommand: func(string, any) (*uds.Response, error) {
			return nil, fmt.Errorf("no socket")
		}}
	}

	// Hold the lock so stopDaemon cannot acquire it
	lockPath := filepath.Join(maestroDir, "locks", "daemon.lock")
	fl := lock.NewFileLock(lockPath)
	if err := fl.TryLock(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fl.Unlock() })

	err := cfg.stopDaemon(maestroDir)
	if err == nil {
		t.Fatal("expected error when lock held and timeout expires")
	}
	if !strings.Contains(err.Error(), "lock still held") {
		t.Errorf("error %q should mention lock still held", err.Error())
	}
}

// --- restoreServerOptions test ---

func TestRestoreServerOptions_NoBackupFile(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	// No crash expected when backup file is missing
	restoreServerOptions(maestroDir)
}

// --- validateAndRecoverYAML tests ---

func TestValidateAndRecoverYAML_EmptyDirs(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)
	// No crash expected when directories are empty
	validateAndRecoverYAML(maestroDir)
}

func TestValidateAndRecoverYAML_ValidFiles(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)

	// Create valid queue file
	queueContent := "schema_version: 1\nfile_type: queue_task\n"
	os.WriteFile(filepath.Join(maestroDir, "queue", "worker1.yaml"), []byte(queueContent), 0644)

	// Create valid results file
	resultsContent := "schema_version: 1\nfile_type: result_task\n"
	os.WriteFile(filepath.Join(maestroDir, "results", "task1.yaml"), []byte(resultsContent), 0644)

	validateAndRecoverYAML(maestroDir)
}

func TestValidateAndRecoverYAML_CorruptFile(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)

	// Create corrupt YAML in queue
	os.WriteFile(filepath.Join(maestroDir, "queue", "worker1.yaml"), []byte("{{invalid"), 0644)

	// Should not panic; may print warnings
	validateAndRecoverYAML(maestroDir)
}

func TestValidateAndRecoverYAML_StateLevelFiles(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)

	// Create valid continuous.yaml
	contContent := "schema_version: 1\nfile_type: state_continuous\nstatus: stopped\n"
	os.WriteFile(filepath.Join(maestroDir, "state", "continuous.yaml"), []byte(contContent), 0644)

	// Create valid metrics.yaml
	metricsContent := "schema_version: 1\nfile_type: state_metrics\n"
	os.WriteFile(filepath.Join(maestroDir, "state", "metrics.yaml"), []byte(metricsContent), 0644)

	validateAndRecoverYAML(maestroDir)
}

func TestValidateAndRecoverYAML_SkipsNonYAMLFiles(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)

	// Non-YAML files should be ignored
	os.WriteFile(filepath.Join(maestroDir, "queue", "readme.txt"), []byte("not yaml"), 0644)
	os.WriteFile(filepath.Join(maestroDir, "results", "data.json"), []byte("{}"), 0644)

	validateAndRecoverYAML(maestroDir)
}

func TestValidateAndRecoverYAML_SkipsSubdirectories(t *testing.T) {
	t.Parallel()
	maestroDir := setupTestMaestroDir(t)

	// Subdirectories should be skipped
	os.MkdirAll(filepath.Join(maestroDir, "queue", "subdir"), 0755)

	validateAndRecoverYAML(maestroDir)
}

// --- hasOtherMaestroSessions tests ---

func TestHasOtherMaestroSessions_NoTmuxServer(t *testing.T) {
	t.Parallel()
	// When tmux server isn't running, ListSessions returns ErrTmuxServer.
	// Since this test environment may not have tmux running at all,
	// we just verify the function doesn't panic.
	_ = hasOtherMaestroSessions()
}

// --- Interface contract verification ---

func TestOsProcessManager_ImplementsProcessManager(t *testing.T) {
	t.Parallel()
	var _ processManager = &osProcessManager{}
}
