package daemonapi

import (
	"fmt"
	"path/filepath"
	"sync"

	"github.com/msageha/maestro_v2/internal/uds"
)

type DashboardUpdater func(maestroDir string) error
type Logf func(format string, args ...any)

type Dashboard struct {
	maestroDir   string
	handlerReady func() bool
	update       DashboardUpdater
	logErrorf    Logf
	mu           sync.Mutex
}

func NewDashboard(maestroDir string, handlerReady func() bool, update DashboardUpdater, logErrorf Logf) *Dashboard {
	return &Dashboard{
		maestroDir:   maestroDir,
		handlerReady: handlerReady,
		update:       update,
		logErrorf:    logErrorf,
	}
}

func (h *Dashboard) SetHandlerReady(fn func() bool) {
	h.handlerReady = fn
}

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
