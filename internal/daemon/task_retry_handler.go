package daemon

import (
	"github.com/msageha/maestro_v2/internal/daemon/retry"
)

// TaskRetryHandler is an alias for retry.Handler.
type TaskRetryHandler = retry.Handler

// NewTaskRetryHandler delegates to retry.NewHandler.
var NewTaskRetryHandler = retry.NewHandler

// NewTaskRetryHandlerWithDeps delegates to retry.NewHandlerWithDeps.
var NewTaskRetryHandlerWithDeps = retry.NewHandlerWithDeps
