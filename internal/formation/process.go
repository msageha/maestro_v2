package formation

import (
	"fmt"
	"syscall"
	"time"
)

// terminateResult indicates the outcome of a terminateProcess call.
type terminateResult int

const (
	// terminateStopped means the target process was confirmed stopped.
	terminateStopped terminateResult = iota
	// terminateNotTarget means the PID no longer belongs to the original
	// target process (e.g. PID was reused). The caller should not clean up
	// PID files in this case.
	terminateNotTarget
)

// terminateProcess sends SIGTERM to pid, waits up to termTimeout for exit,
// then escalates to SIGKILL. Before each signal, sameProcess is called to
// verify the PID still belongs to the intended target (mitigating PID reuse).
//
// Returns terminateNotTarget if sameProcess returns false at any check point.
// Returns an error only if the process survives SIGKILL.
func terminateProcess(pid int, sameProcess func(int) bool, termTimeout time.Duration) (terminateResult, error) {
	if !processAlive(pid) {
		return terminateStopped, nil
	}

	// Verify identity before SIGTERM
	if !sameProcess(pid) {
		return terminateNotTarget, nil
	}
	_ = procMgr.Signal(pid, syscall.SIGTERM)

	// Poll for exit
	termDeadline := time.Now().Add(termTimeout)
	for time.Now().Before(termDeadline) {
		if !processAlive(pid) {
			return terminateStopped, nil
		}
		time.Sleep(processExitPollInterval)
	}

	// Verify identity before SIGKILL
	if !sameProcess(pid) {
		return terminateNotTarget, nil
	}
	if processAlive(pid) {
		_ = procMgr.Signal(pid, syscall.SIGKILL)
		time.Sleep(postSignalWait)
	}

	if processAlive(pid) {
		return terminateStopped, fmt.Errorf("process pid=%d still alive after SIGKILL", pid)
	}
	return terminateStopped, nil
}
