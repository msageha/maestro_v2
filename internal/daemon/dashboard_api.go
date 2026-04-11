package daemon

import (
	"fmt"
	"path/filepath"
	"sync"

	"github.com/msageha/maestro_v2/internal/uds"
)

// DashboardAPI handles the "dashboard" UDS endpoint.
type DashboardAPI struct {
	*apiContext
	handlerReady func() bool // returns true when QueueHandler is initialized
	dashboardMu  sync.Mutex
}

func (h *DashboardAPI) handleDashboard(_ *uds.Request) *uds.Response {
	if !h.handlerReady() {
		return uds.ErrorResponse(uds.ErrCodeInternal, "handler not initialized")
	}

	h.dashboardMu.Lock()
	defer h.dashboardMu.Unlock()

	formatter := NewDashboardFormatter(h.maestroDir)
	if err := formatter.UpdateDashboardFile(); err != nil {
		h.logFn(LogLevelError, "dashboard regeneration error=%v", err)
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("dashboard generation failed: %v", err))
	}

	dashboardPath := filepath.Join(h.maestroDir, "dashboard.md")
	return uds.SuccessResponse(map[string]string{
		"status": "regenerated",
		"path":   dashboardPath,
	})
}
