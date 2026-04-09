package model

import "errors"

// Sentinel errors for common not-found conditions.
var (
	ErrWorkerNotFound = errors.New("worker not found")
	ErrTaskNotFound   = errors.New("task not found")
)
