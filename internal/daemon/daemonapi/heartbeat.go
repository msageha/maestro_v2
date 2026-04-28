package daemonapi

import (
	"encoding/json"
	"sync"

	"github.com/msageha/maestro_v2/internal/uds"
)

// RequestHandler is the minimal interface a heartbeat backend must
// implement: it receives the raw JSON params already split out from the
// UDS envelope and returns the response to forward verbatim.
type RequestHandler interface {
	Handle(params json.RawMessage) *uds.Response
}

// HandlerFactory builds a RequestHandler on demand. Heartbeat caches
// the produced handler so reconfiguration via SetFactory takes effect on
// the next request without recreating the handler on every call.
type HandlerFactory func() RequestHandler

// Heartbeat dispatches heartbeat UDS requests through a lazily-built
// RequestHandler. The factory indirection lets daemon startup swap the
// implementation after construction without racing in-flight requests.
type Heartbeat struct {
	factory HandlerFactory
	mu      sync.Mutex
	handler RequestHandler
}

// NewHeartbeat constructs a Heartbeat that lazily instantiates its
// handler from factory on the first request.
func NewHeartbeat(factory HandlerFactory) *Heartbeat {
	return &Heartbeat{factory: factory}
}

// SetFactory replaces the handler factory and discards any cached
// handler so the next Handle call rebuilds against the new factory.
func (h *Heartbeat) SetFactory(factory HandlerFactory) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.factory = factory
	h.handler = nil
}

// Handle routes the heartbeat request to the cached RequestHandler. A
// missing factory or nil handler is reported as an internal error so the
// caller can surface "handler not initialized" rather than silently
// dropping the request.
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
