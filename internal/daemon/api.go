package daemon

import (
	"fmt"
	"path/filepath"
	"sync"

	"github.com/msageha/maestro_v2/internal/events"
	"github.com/msageha/maestro_v2/internal/uds"
)

// API groups all UDS request handler methods for the daemon.
// It holds a back-pointer to Daemon for access to shared state.
type API struct {
	d           *Daemon
	dashboardMu sync.Mutex // serializes concurrent dashboard generation
}

// registerHandlers registers UDS request handlers on the daemon's server.
func (a *API) registerHandlers() {
	d := a.d
	d.server.Handle("ping", func(req *uds.Request) *uds.Response {
		return uds.SuccessResponse(map[string]string{"status": "ok"})
	})

	d.server.Handle("scan", func(req *uds.Request) *uds.Response {
		if d.handler == nil {
			return uds.ErrorResponse(uds.ErrCodeInternal, "handler not initialized")
		}
		d.handler.PeriodicScanWithContext(d.ctx)
		return uds.SuccessResponse(map[string]string{"status": "scanned"})
	})

	d.server.Handle("shutdown", func(req *uds.Request) *uds.Response {
		d.log(LogLevelInfo, "shutdown requested via UDS")
		go func() { defer d.recoverPanic("shutdownHandler"); d.Shutdown() }()
		return uds.SuccessResponse(map[string]string{"status": "shutdown_accepted"})
	})

	d.server.Handle("queue_write", a.handleQueueWrite)
	d.server.Handle("result_write", a.handleResultWrite)
	d.server.Handle("task_heartbeat", a.handleTaskHeartbeat)
	d.server.Handle("plan", a.handlePlan)
	d.server.Handle("dashboard", a.handleDashboard)
}

// handleTaskHeartbeat handles task heartbeat requests.
func (a *API) handleTaskHeartbeat(req *uds.Request) *uds.Response {
	d := a.d
	if d.handler == nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, "handler not initialized")
	}
	heartbeatHandler := NewTaskHeartbeatHandler(
		d.maestroDir,
		d.config,
		d.handler.leaseManager,
		d.logger,
		d.logLevel,
		&d.handler.scanMu,
		d.lockMap,
	)
	return heartbeatHandler.Handle(req.Params)
}

// handleDashboard triggers dashboard regeneration and returns the result.
// DashboardFormatter reads state from on-disk YAML files (not in-memory scan
// state), so it does not require scanMu. Holding scanMu here would block
// PeriodicScan for the duration of the file I/O. A dedicated dashboardMu
// serializes concurrent dashboard writes to prevent temp-file clobbering.
func (a *API) handleDashboard(req *uds.Request) *uds.Response {
	d := a.d
	if d.handler == nil {
		return uds.ErrorResponse(uds.ErrCodeInternal, "handler not initialized")
	}

	a.dashboardMu.Lock()
	defer a.dashboardMu.Unlock()

	// Use the new dashboard formatter for human-readable output
	formatter := NewDashboardFormatter(d.maestroDir)
	if err := formatter.UpdateDashboardFile(); err != nil {
		d.log(LogLevelError, "dashboard regeneration error=%v", err)
		return uds.ErrorResponse(uds.ErrCodeInternal, fmt.Sprintf("dashboard generation failed: %v", err))
	}

	dashboardPath := filepath.Join(d.maestroDir, "dashboard.md")
	return uds.SuccessResponse(map[string]string{
		"status": "regenerated",
		"path":   dashboardPath,
	})
}

// acquireFileLock acquires the shared file mutex to serialize with QueueHandler's PeriodicScan.
func (a *API) acquireFileLock() {
	if a.d.handler != nil {
		a.d.handler.LockFiles()
	}
}

// releaseFileLock releases the shared file mutex.
func (a *API) releaseFileLock() {
	if a.d.handler != nil {
		a.d.handler.UnlockFiles()
	}
}

// notifySelfWrite records a self-write for fsnotify filtering and publishes
// an EventQueueWritten event to trigger processing via the event bus.
// data is the object that was written (used to compute content hash).
func (a *API) notifySelfWrite(queuePath, writeType string, data any) {
	a.d.selfWrites.Record(queuePath, data)
	bus := a.d.eventBus
	if bus != nil {
		bus.Publish(events.EventQueueWritten, map[string]interface{}{
			"file":   filepath.Base(queuePath),
			"source": "uds",
			"type":   writeType,
		})
	}
}

// recordSelfWrite records a self-write for fsnotify filtering without
// publishing an event (used when the caller already triggers processing directly).
// data is the object that was written (used to compute content hash).
func (a *API) recordSelfWrite(path string, data any) {
	a.d.selfWrites.Record(path, data)
}

// --- Forwarding methods on Daemon for backward compatibility ---
// These allow tests and internal code to call d.handleXxx() directly.

func (d *Daemon) handleQueueWrite(req *uds.Request) *uds.Response {
	return d.api.handleQueueWrite(req)
}

func (d *Daemon) handleResultWrite(req *uds.Request) *uds.Response {
	return d.api.handleResultWrite(req)
}

func (d *Daemon) handlePlan(req *uds.Request) *uds.Response {
	return d.api.handlePlan(req)
}

func (d *Daemon) handleDashboard(req *uds.Request) *uds.Response {
	return d.api.handleDashboard(req)
}

func (d *Daemon) handleTaskHeartbeat(req *uds.Request) *uds.Response {
	return d.api.handleTaskHeartbeat(req)
}

func (d *Daemon) notifySelfWrite(queuePath, writeType string, data any) {
	d.api.notifySelfWrite(queuePath, writeType, data)
}

func (d *Daemon) recordSelfWrite(path string, data any) {
	d.api.recordSelfWrite(path, data)
}

func (d *Daemon) writeLearnings(params ResultWriteParams, resultID string) error {
	return d.api.writeLearnings(params, resultID)
}
