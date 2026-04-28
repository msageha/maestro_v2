package daemonapi

import (
	"fmt"
	"path/filepath"
	"sync"

	"github.com/msageha/maestro_v2/internal/uds"
)

// DashboardUpdater regenerates dashboard.md under maestroDir.
type DashboardUpdater func(maestroDir string) error

// Logf is a printf-shaped logging hook injected into API handlers so they
// can emit structured warnings without depending on a concrete logger.
type Logf func(format string, args ...any)

// Dashboard handles dashboard regeneration UDS requests.
type Dashboard struct {
	maestroDir   string
	handlerReady func() bool
	update       DashboardUpdater
	logErrorf    Logf
	mu           sync.Mutex
}

// NewDashboard constructs a Dashboard handler with the provided
// dependencies. handlerReady is consulted on every request so dashboard
// requests arriving before initComponents finishes are rejected rather
// than silently regenerating against a half-wired daemon.
func NewDashboard(maestroDir string, handlerReady func() bool, update DashboardUpdater, logErrorf Logf) *Dashboard {
	return &Dashboard{
		maestroDir:   maestroDir,
		handlerReady: handlerReady,
		update:       update,
		logErrorf:    logErrorf,
	}
}

// SetHandlerReady replaces the readiness predicate. Used by daemon
// startup to swap the bootstrap "always false" gate for the real one.
func (h *Dashboard) SetHandlerReady(fn func() bool) {
	h.handlerReady = fn
}

// Handle implements the dashboard regeneration request: it runs the
// configured DashboardUpdater under a per-handler mutex and returns the
// regenerated dashboard.md path on success.
func (h *Dashboard) Handle(_ *uds.Request) *uds.Response {
	if h.handlerReady == nil || !h.handlerReady() {
		return uds.ErrorResponse(uds.ErrCodeInternal, "handler not initialized")
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if err := h.update(h.maestroDir); err != nil {
		if h.logErrorf != nil {
			h.logErrorf("dashboard regeneration error=%v", err)
		}
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("dashboard generation failed: %v", err))
	}

	dashboardPath := filepath.Join(h.maestroDir, "dashboard.md")
	return uds.SuccessResponse(map[string]string{
		"status": "regenerated",
		"path":   dashboardPath,
	})
}
