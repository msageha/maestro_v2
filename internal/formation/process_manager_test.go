package formation

import (
	"os"
	"syscall"
	"testing"
)

func TestOsProcessManager_Alive_CurrentProcess(t *testing.T) {
	t.Parallel()
	pm := &osProcessManager{}

	// Current process should be alive
	if !pm.Alive(os.Getpid()) {
		t.Error("expected current process to be alive")
	}
}

func TestOsProcessManager_Alive_InvalidPID(t *testing.T) {
	t.Parallel()
	pm := &osProcessManager{}

	tests := []struct {
		name string
		pid  int
	}{
		{"zero", 0},
		{"negative", -1},
		{"large negative", -99999},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if pm.Alive(tt.pid) {
				t.Errorf("expected Alive(%d) to return false", tt.pid)
			}
		})
	}
}

func TestOsProcessManager_Alive_NonExistentPID(t *testing.T) {
	t.Parallel()
	pm := &osProcessManager{}

	// Very large PID is unlikely to exist
	if pm.Alive(99999999) {
		t.Error("expected non-existent PID to not be alive")
	}
}

func TestOsProcessManager_StartTime_CurrentProcess(t *testing.T) {
	t.Parallel()
	pm := &osProcessManager{}

	st := pm.StartTime(os.Getpid())
	// On macOS/Linux, current process should have a start time
	if st == "" {
		t.Skip("start time not available on this platform")
	}
}

func TestOsProcessManager_StartTime_InvalidPID(t *testing.T) {
	t.Parallel()
	pm := &osProcessManager{}

	if st := pm.StartTime(-1); st != "" {
		t.Errorf("expected empty start time for invalid PID, got %q", st)
	}
	if st := pm.StartTime(99999999); st != "" {
		t.Errorf("expected empty start time for non-existent PID, got %q", st)
	}
}

func TestOsProcessManager_Signal_CurrentProcess(t *testing.T) {
	t.Parallel()
	pm := &osProcessManager{}

	// Signal 0 checks process existence without sending a real signal
	if err := pm.Signal(os.Getpid(), syscall.Signal(0)); err != nil {
		t.Errorf("expected nil error for Signal(0) to current process, got %v", err)
	}
}

func TestOsProcessManager_Signal_NonExistentPID(t *testing.T) {
	t.Parallel()
	pm := &osProcessManager{}

	err := pm.Signal(99999999, syscall.Signal(0))
	if err == nil {
		t.Error("expected error for Signal to non-existent PID")
	}
}

func TestOsProcessManager_Signal_NegativePID(t *testing.T) {
	t.Parallel()
	pm := &osProcessManager{}

	// Negative PID should return error (or be handled by OS)
	err := pm.Signal(-1, syscall.Signal(0))
	// -1 sends to all processes the caller has permission to signal,
	// which may or may not error. Just ensure no panic.
	_ = err
}
