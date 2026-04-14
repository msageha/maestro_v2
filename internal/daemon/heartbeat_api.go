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

	// Cached handler — lazily initialized on first request with non-nil deps.
	initMu  sync.Mutex
	handler *TaskHeartbeatHandler
}

// cachedHandler returns the singleton TaskHeartbeatHandler, creating it on the
// first call where late-bound dependencies are available. Returns nil if deps
// are not yet wired.
func (h *HeartbeatAPI) cachedHandler() *TaskHeartbeatHandler {
	h.initMu.Lock()
	defer h.initMu.Unlock()
	if h.handler != nil {
		return h.handler
	}
	lm := h.leaseManager()
	mu := h.scanMu()
	if lm == nil || mu == nil {
		return nil
	}
	h.handler = NewTaskHeartbeatHandler(
		h.maestroDir,
		*h.config,
		lm,
		h.logger,
		h.logLevel,
		mu,
		h.lockMap,
	)
	return h.handler
}

func (h *HeartbeatAPI) handleTaskHeartbeat(req *uds.Request) *uds.Response {
	handler := h.cachedHandler()
	if handler == nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, "handler not initialized")
	}
	return handler.Handle(req.Params)
}
