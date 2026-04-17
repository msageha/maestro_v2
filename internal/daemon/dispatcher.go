package daemon

// This file re-exports types from internal/daemon/dispatch so that existing code
// referencing daemon.Dispatcher, daemon.EffectivePriority, etc. continues to compile.

import (
	"log"

	"github.com/msageha/maestro_v2/internal/daemon/dispatch"
	"github.com/msageha/maestro_v2/internal/model"
)

// Dispatcher is an alias for dispatch.Dispatcher.
type Dispatcher = dispatch.Dispatcher

// EffectivePriority is re-exported from dispatch.
var EffectivePriority = dispatch.EffectivePriority

// NewDispatcher creates a new Dispatcher with backward-compatible signature.
// The leaseManager parameter is accepted for API compatibility but is unused
// by the Dispatcher (lease operations are handled at the QueueHandler level).
func NewDispatcher(maestroDir string, cfg model.Config, _ QueueLeaseManager, logger *log.Logger, logLevel LogLevel, ep ExecutorGetter, clock Clock) *Dispatcher {
	return dispatch.New(maestroDir, cfg, logger, logLevel, ep, clock)
}
