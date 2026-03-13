package formation

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// processStartTime returns a monotonic token representing when pid was started.
// On Linux this reads field 22 (starttime) from /proc/<pid>/stat.
// Returns "" if the process info is unavailable.
func processStartTime(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	// Format: pid (comm) state fields...
	// Field 22 is starttime (1-indexed). Find end of comm (last ')').
	s := string(data)
	idx := strings.LastIndex(s, ")")
	if idx < 0 || idx+2 >= len(s) {
		return ""
	}
	fields := strings.Fields(s[idx+2:])
	// starttime is field 22 overall; after comm end, index 0 = field 3, so starttime = index 19
	if len(fields) < 20 {
		return ""
	}
	starttime := fields[19]
	// Validate it's numeric
	if _, err := strconv.ParseUint(starttime, 10, 64); err != nil {
		return ""
	}
	return starttime
}
