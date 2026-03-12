package formation

import (
	"fmt"
	"syscall"
	"time"
)

// TerminateResult indicates the outcome of a terminateProcess call.
type TerminateResult int

const (
	// TerminateStopped means the target process was confirmed stopped.
	TerminateStopped TerminateResult = iota
	// TerminateNotTarget means the PID no longer belongs to the original
	// target process (e.g. PID was reused). The caller should not clean up
	// PID files in this case.
	TerminateNotTarget
)

// terminateProcess sends SIGTERM to pid, waits up to termTimeout for exit,
// then escalates to SIGKILL. Before each signal, sameProcess is called to
// verify the PID still belongs to the intended target (mitigating PID reuse).
//
// Returns TerminateNotTarget if sameProcess returns false at any check point.
// Returns an error only if the process survives SIGKILL.
func terminateProcess(pid int, sameProcess func(int) bool, termTimeout time.Duration) (TerminateResult, error) {
	if !processAlive(pid) {
		return TerminateStopped, nil
	}

	// Verify identity before SIGTERM
	if !sameProcess(pid) {
		return TerminateNotTarget, nil
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)

	// Poll for exit
	termDeadline := time.Now().Add(termTimeout)
	for time.Now().Before(termDeadline) {
		if !processAlive(pid) {
			return TerminateStopped, nil
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Verify identity before SIGKILL
	if !sameProcess(pid) {
		return TerminateNotTarget, nil
	}
	if processAlive(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		time.Sleep(500 * time.Millisecond)
	}

	if processAlive(pid) {
		return TerminateStopped, fmt.Errorf("process pid=%d still alive after SIGKILL", pid)
	}
	return TerminateStopped, nil
}
