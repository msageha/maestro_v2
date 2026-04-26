package daemon

import (
	"context"
	"log"
	"path/filepath"

	"github.com/msageha/maestro_v2/internal/events"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
)

// fileLockHolder abstracts the shared file mutex provided by QueueHandler.
// API handlers call these to serialize with PeriodicScan.
type fileLockHolder interface {
	LockFiles()
	UnlockFiles()
}

// apiContext bundles dependencies shared across all UDS API domain handlers.
// It replaces the direct *Daemon back-pointer with an explicit dependency set.
//
// Late-bound fields (eventBus, fileLockHolder) are populated during
// initComponents after the QueueHandler and EventBus are created.
type apiContext struct {
	maestroDir string
	config     *model.Config // pointer to Daemon.config so test mutations are visible
	clock      Clock
	lockMap    *lock.MutexMap
	logFn      logFunc
	logger     *log.Logger
	logLevel   LogLevel
	selfWrites *selfWriteTracker
	fileStore  ResultFileStore
	spawnTask  backgroundTaskSpawner

	// Late-bound (set via setters after initComponents)
	eventBus       func() *events.Bus // lazy accessor; reads Daemon.eventBus at call time
	fileLockHolder fileLockHolder
}

// logFunc is defined in phase_c_manager.go — reused here.

// SetEventBus wires a lazy event bus accessor for self-write notifications.
// The accessor is called on each publish so that test mutations to
// Daemon.eventBus are visible without an explicit re-wire.
func (c *apiContext) SetEventBus(fn func() *events.Bus) {
	c.eventBus = fn
}

// SetFileLockHolder wires the shared file mutex.
func (c *apiContext) SetFileLockHolder(h fileLockHolder) {
	c.fileLockHolder = h
}

// acquireFileLock acquires the shared file mutex to serialize with QueueHandler's PeriodicScan.
// fileLockHolder is a late-bound dependency set via SetFileLockHolder after initComponents.
// A nil value indicates the lock has not been wired yet, which is a programming error
// in production but expected in unit tests that bypass initComponents.
func (c *apiContext) acquireFileLock() {
	if c.fileLockHolder == nil {
		c.logFn(LogLevelWarn, "acquireFileLock called but fileLockHolder is nil (not wired); skipping lock")
		return
	}
	c.fileLockHolder.LockFiles()
}

// releaseFileLock releases the shared file mutex.
func (c *apiContext) releaseFileLock() {
	if c.fileLockHolder == nil {
		c.logFn(LogLevelWarn, "releaseFileLock called but fileLockHolder is nil (not wired); skipping unlock")
		return
	}
	c.fileLockHolder.UnlockFiles()
}

// notifySelfWrite records a self-write for fsnotify filtering and publishes
// an EventQueueWritten event to trigger processing via the event bus.
func (c *apiContext) notifySelfWrite(queuePath, writeType string, data any) {
	c.selfWrites.Record(queuePath, data)
	if bus := c.getEventBus(); bus != nil {
		bus.Publish(events.EventQueueWritten, map[string]interface{}{
			"file":   filepath.Base(queuePath),
			"source": "uds",
			"type":   writeType,
		})
	}
}

// publishQueueWritten publishes an EventQueueWritten event to trigger an
// immediate queue scan without recording a self-write hash.
func (c *apiContext) publishQueueWritten(source string) {
	if bus := c.getEventBus(); bus != nil {
		bus.Publish(events.EventQueueWritten, map[string]interface{}{
			"source": source,
			"type":   "plan",
		})
	}
}

// getEventBus resolves the lazy event bus accessor. Returns nil if not wired.
func (c *apiContext) getEventBus() *events.Bus {
	if c.eventBus != nil {
		return c.eventBus()
	}
	return nil
}

// recordSelfWrite records a self-write for fsnotify filtering without
// publishing an event.
func (c *apiContext) recordSelfWrite(path string, data any) {
	c.selfWrites.Record(path, data)
}

// scanTriggerFunc is called after result_write to trigger an async queue scan.
// It encapsulates Daemon.spawnTracked + QueueHandler.PeriodicScanWithContext.
type scanTriggerFunc func(ctx context.Context)

// backgroundTaskSpawner starts daemon-owned background work and returns false
// when the daemon is shutting down and the task was not admitted.
type backgroundTaskSpawner func(name string, fn func(context.Context)) bool
