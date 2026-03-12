package formation

import (
	"fmt"
	"os/exec"
	"strings"
)

// processStartTime returns a token representing when pid was started.
// On macOS this uses `ps -o lstart= -p <pid>` to get the launch timestamp.
// Returns "" if the process info is unavailable.
func processStartTime(pid int) string {
	out, err := exec.Command("ps", "-o", "lstart=", "-p", fmt.Sprintf("%d", pid)).Output()
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return ""
	}
	return s
}
