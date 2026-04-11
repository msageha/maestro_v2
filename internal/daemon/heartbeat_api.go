package daemon

import (
	"log"
	"sync"

	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
	"github.com/msageha/maestro_v2/internal/uds"
)

// HeartbeatAPI handles the "task_heartbeat" UDS endpoint.
type HeartbeatAPI struct {
	maestroDir string
	config     *model.Config
	logger     *log.Logger
	logLevel   LogLevel
	lockMap    *lock.MutexMap
	// Late-bound deps from QueueHandler
	leaseManager func() QueueLeaseManager
	scanMu       func() *sync.RWMutex
}

func (h *HeartbeatAPI) handleTaskHeartbeat(req *uds.Request) *uds.Response {
	lm := h.leaseManager()
	mu := h.scanMu()
	if lm == nil || mu == nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, "handler not initialized")
	}
	handler := NewTaskHeartbeatHandler(
		h.maestroDir,
		*h.config,
		lm,
		h.logger,
		h.logLevel,
		mu,
		h.lockMap,
	)
	return handler.Handle(req.Params)
}
