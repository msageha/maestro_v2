package bridge

import "errors"

// Sentinel errors for bridge operations.
var (
	ErrMarshalPlanResult = errors.New("failed to marshal plan result")
)
