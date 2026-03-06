package daemon

import (
	"github.com/msageha/maestro_v2/internal/daemon/continuous"
)

// ContinuousHandler is an alias for continuous.Handler.
type ContinuousHandler = continuous.Handler

// NewContinuousHandler delegates to continuous.NewHandler.
var NewContinuousHandler = continuous.NewHandler
