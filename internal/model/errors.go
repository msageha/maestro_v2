package model

import "errors"

// Sentinel errors for common not-found conditions.
var (
	ErrWorkerNotFound = errors.New("worker not found")
	ErrTaskNotFound   = errors.New("task not found")

	// ErrStateNotFound is returned by StateReader methods when the state file does not exist
	// (i.e., the command has not been submitted yet). Callers can use errors.Is to distinguish
	// this from other read errors (e.g., parse failures on an existing file).
	ErrStateNotFound = errors.New("command state file not found")

	// ErrPhaseNotFound is returned when a specific phase ID is not present in a command's
	// phase list. Unlike ErrStateNotFound (state file missing), this means the state file
	// exists but the requested phase does not.
	ErrPhaseNotFound = errors.New("command phase not found")
)
