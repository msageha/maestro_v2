package formation

import (
	"fmt"
	"log"
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
func (c *Config) terminateProcess(pid int, sameProcess func(int) bool, termTimeout time.Duration) (terminateResult, error) {
	if !c.ProcMgr.Alive(pid) {
		return terminateStopped, nil
	}

	// Verify identity before SIGTERM
	if !sameProcess(pid) {
		return terminateNotTarget, nil
	}
	if err := c.ProcMgr.Signal(pid, syscall.SIGTERM); err != nil {
		log.Printf("[WARN] SIGTERM pid=%d: %v", pid, err)
	}

	// Poll for exit
	termDeadline := time.Now().Add(termTimeout)
	for time.Now().Before(termDeadline) {
		if !c.ProcMgr.Alive(pid) {
			return terminateStopped, nil
		}
		time.Sleep(c.ProcessExitPollInterval)
	}

	// Verify identity before SIGKILL
	if !sameProcess(pid) {
		return terminateNotTarget, nil
	}
	if c.ProcMgr.Alive(pid) {
		if err := c.ProcMgr.Signal(pid, syscall.SIGKILL); err != nil {
			log.Printf("[WARN] SIGKILL pid=%d: %v", pid, err)
		}
		time.Sleep(c.PostSignalWait)
	}

	if c.ProcMgr.Alive(pid) {
		return terminateStopped, fmt.Errorf("process pid=%d still alive after SIGKILL", pid)
	}
	return terminateStopped, nil
}

func terminateProcess(pid int, sameProcess func(int) bool, termTimeout time.Duration) (terminateResult, error) {
	return defaultConfig.terminateProcess(pid, sameProcess, termTimeout)
}
