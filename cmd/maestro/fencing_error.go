package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/msageha/maestro_v2/internal/uds"
)

// fencingErrorEnvelope is the single-line JSON shape written to stderr for
// fencing-related daemon rejects. F-019 step 2.
//
// The discriminator key is `maestro_error`; its value is the UDS error code
// (e.g. "FENCING_REJECT_EPOCH"). Worker shell wrappers can identify the
// structured line either by `grep -F maestro_error` or by `jq -r
// .maestro_error` — the value carries enough information to branch even
// without parsing `details`.
type fencingErrorEnvelope struct {
	Code    string             `json:"maestro_error"`
	Details uds.FencingDetails `json:"details"`
}

// classifyFencingExitCode maps a UDS error response to the structured exit
// code introduced by F-019. Returns 0 when the response does not represent
// a fencing reject the CLI should surface with a typed exit code; the
// caller falls back to ExitCodeRetryable / 1 in that case.
//
// The dispatch is intentionally driven by the structured Details.Kind first
// (set by the daemon since F-019 step 1) so legacy / unparseable details
// fall through gracefully to UDS error code matching, which preserves the
// pre-F-019 behaviour for older daemons.
func classifyFencingExitCode(resp *uds.Response) int {
	if resp == nil || resp.Error == nil {
		return 0
	}
	// Prefer the structured kind when the daemon has attached it.
	if d, ok := decodeFencingDetails(resp); ok {
		switch d.Kind {
		case "fencing_epoch_mismatch":
			return ExitCodeFencingEpoch
		case "fencing_status_mismatch":
			return ExitCodeFencingStatus
		case "max_runtime_exceeded":
			return ExitCodeMaxRuntimeExceeded
		}
	}
	// Fallback: map from the legacy UDS error code.
	switch resp.Error.Code {
	case uds.ErrCodeFencingRejectEpoch:
		return ExitCodeFencingEpoch
	case uds.ErrCodeFencingRejectStatus, uds.ErrCodeFencingReject:
		return ExitCodeFencingStatus
	case uds.ErrCodeMaxRuntimeExceeded:
		return ExitCodeMaxRuntimeExceeded
	}
	return 0
}

// decodeFencingDetails returns the FencingDetails payload attached to resp,
// or (zero, false) when no parseable payload is present. This is a thin
// wrapper so callers do not need to reach into resp.Error.Details directly.
func decodeFencingDetails(resp *uds.Response) (uds.FencingDetails, bool) {
	raw := resp.ErrorDetails()
	if len(raw) == 0 {
		return uds.FencingDetails{}, false
	}
	var d uds.FencingDetails
	if err := json.Unmarshal(raw, &d); err != nil {
		return uds.FencingDetails{}, false
	}
	if d.Kind == "" {
		return uds.FencingDetails{}, false
	}
	return d, true
}

// emitFencingStderrEnvelope writes a single JSON line to stderr (or the
// supplied writer in tests) with the structured payload. Returns true when
// it actually wrote a line so the caller can decide whether the legacy
// Message-style line is still useful (we always emit both for a transition
// window — see codex consultation in F-019 step 1).
func emitFencingStderrEnvelope(resp *uds.Response, w io.Writer) bool {
	d, ok := decodeFencingDetails(resp)
	if !ok {
		return false
	}
	env := fencingErrorEnvelope{
		Code:    resp.Error.Code,
		Details: d,
	}
	out, err := json.Marshal(env)
	if err != nil {
		return false
	}
	if w == nil {
		w = os.Stderr
	}
	// Diagnostic output to a TTY/log writer; failure to write is not a
	// recoverable error (the original CLI error is still propagated by the
	// caller), but ignoring the return tripped errcheck.
	_, _ = fmt.Fprintln(w, string(out))
	return true
}

// fencingCLIError builds a CLIError that surfaces a fencing reject with the
// structured exit code, emits the JSON envelope to stderr, and keeps the
// human-readable Message for the legacy stderr line. Callers (heartbeat,
// result write) should use this in place of the previous generic
// CLIError{Code: ExitCodeRetryable, Msg: ...} when classifyFencingExitCode
// returns a non-zero code.
//
// Silent=true is used for max_runtime_exceeded to suppress the legacy
// human-readable stderr line (cmd_task.go); the structured JSON envelope is
// still emitted so wrappers can branch on machine-readable details.
func fencingCLIError(resp *uds.Response, silent bool, prefix string) *CLIError {
	exitCode := classifyFencingExitCode(resp)
	if exitCode == 0 {
		exitCode = ExitCodeRetryable
	}
	emitFencingStderrEnvelope(resp, os.Stderr)
	return &CLIError{
		Code:   exitCode,
		Silent: silent,
		Msg:    fmt.Sprintf("%s: [%s] %s", prefix, resp.Error.Code, resp.Error.Message),
	}
}
