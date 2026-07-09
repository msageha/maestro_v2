package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/msageha/maestro_v2/internal/uds"
)

// printJSONResponse marshals data as indented JSON and writes it to stdout.
// cmd is used for error context (e.g. "plan submit", "result write").
// A success response without a payload prints an empty JSON object instead
// of the literal `null`, which downstream parsers (and LLM agents reading
// the pane) tend to misread as a failure sentinel.
func printJSONResponse(data json.RawMessage, cmd string) error {
	if len(data) == 0 || string(data) == "null" {
		fmt.Println("{}")
		return nil
	}
	out, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("maestro %s: format response json: %w", cmd, err)
	}
	fmt.Println(string(out))
	return nil
}

// udsCLIError converts a failed UDS response into a CLIError honoring the
// exit-code contract documented on ExitCodeRetryable: a BACKPRESSURE
// rejection is retryable and must exit 2 on every daemon-RPC path, so
// Worker shell wrappers and LLM agents can branch on $? uniformly instead
// of grepping stderr. All other codes keep the generic exit 1.
// Fencing-aware commands (task heartbeat, result write) must run
// classifyFencingExitCode first; this helper is their fallback.
func udsCLIError(prefix string, resp *uds.Response) *CLIError {
	code, msg := udsErrorInfo(resp)
	exit := 1
	if code == uds.ErrCodeBackpressure {
		exit = ExitCodeRetryable
	}
	return &CLIError{Code: exit, Msg: fmt.Sprintf("%s: [%s] %s", prefix, code, msg)}
}

// isStdinTerminal reports whether os.Stdin is attached to an interactive
// terminal (character device). Commands that default to reading stdin use
// this to fail fast with an actionable error instead of blocking on
// io.ReadAll until EOF — inside an agent tmux pane that block is silent
// and consumes the whole turn. Declared as a variable so tests can inject
// the terminal case without a pty.
var isStdinTerminal = func() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

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
