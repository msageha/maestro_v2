// Package notify provides desktop notification support.
package notify

import (
	"fmt"
	"os/exec"
	"strings"
)

// Send sends a macOS notification via osascript with sound.
func Send(title, message string) error {
	title = escapeAppleScript(title)
	message = escapeAppleScript(message)

	script := fmt.Sprintf(
		`display notification %q with title %q sound name "default"`,
		message, title,
	)

	cmd := exec.Command("osascript", "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("osascript: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func escapeAppleScript(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}
