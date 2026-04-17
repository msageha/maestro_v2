package plan

import "errors"

// Sentinel errors for plan operations.
var (
	ErrDoubleSubmit      = errors.New("double submit")
	ErrCommandCancelled  = errors.New("command has been cancelled")
	ErrNoAvailableWorker = errors.New("no available worker")
)
