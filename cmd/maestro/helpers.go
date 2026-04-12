package main

import (
	"strings"

	"github.com/msageha/maestro_v2/internal/uds"
)

// sanitizeForTerminal removes control characters to prevent terminal injection
// and output spoofing. Tabs and newlines are replaced with spaces to prevent
// multi-line output spoofing attacks.
func sanitizeForTerminal(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))
	for _, r := range s {
		if r == '\t' || r == '\n' {
			sb.WriteByte(' ')
		} else if r >= 0x20 && r != 0x7f {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// udsErrorInfo extracts the error code and message from a UDS response.
// Returns ("", "unknown error") when resp.Error is nil.
func udsErrorInfo(resp *uds.Response) (code, msg string) {
	if resp.Error != nil {
		return resp.Error.Code, resp.Error.Message
	}
	return "", "unknown error"
}
