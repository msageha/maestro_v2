package daemonapi

import (
	"encoding/json"
	"sync"

	"github.com/msageha/maestro_v2/internal/uds"
)

type RequestHandler interface {
	Handle(params json.RawMessage) *uds.Response
}

type HandlerFactory func() RequestHandler

type Heartbeat struct {
	factory HandlerFactory
	mu      sync.Mutex
	handler RequestHandler
}

func NewHeartbeat(factory HandlerFactory) *Heartbeat {
	return &Heartbeat{factory: factory}
}

func (h *Heartbeat) SetFactory(factory HandlerFactory) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.factory = factory
	h.handler = nil
}

func (h *Heartbeat) Handle(req *uds.Request) *uds.Response {
	handler := h.cachedHandler()
	if handler == nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, "handler not initialized")
	}
	return handler.Handle(req.Params)
}

func (h *Heartbeat) cachedHandler() RequestHandler {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.handler != nil {
		return h.handler
	}
	if h.factory == nil {
		return nil
	}
	h.handler = h.factory()
	return h.handler
}
