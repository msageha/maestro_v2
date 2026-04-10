package daemon

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

// ExecutorProvider encapsulates lazy initialization, caching, and lifecycle
// management of a shared AgentExecutor instance. It replaces the duplicated
// executor cache patterns in Dispatcher, ResultHandler, and CancelHandler.
//
// On initialization failure, retries with exponential backoff (1s→2s→4s→8s→16s cap).
// Success resets the backoff state. SetFactory resets all cached state.
type ExecutorProvider struct {
	maestroDir string
	watcherCfg model.WatcherConfig
	logLevel   string
	clock      Clock

	mu             sync.Mutex
	factory        ExecutorFactory
	cachedExec     AgentExecutor
	execInit       bool
	execFailCount  int
	execLastFailAt time.Time
}

// NewExecutorProvider creates an ExecutorProvider with retry-backoff error handling.
func NewExecutorProvider(
	maestroDir string,
	watcherCfg model.WatcherConfig,
	logLevel string,
	factory ExecutorFactory,
	clock Clock,
) *ExecutorProvider {
	return &ExecutorProvider{
		maestroDir: maestroDir,
		watcherCfg: watcherCfg,
		logLevel:   logLevel,
		factory:    factory,
		clock:      clock,
	}
}

// GetExecutor returns the shared executor instance, creating it lazily on first call.
// On failure, subsequent calls retry with exponential backoff (1s→2s→4s→8s→16s cap).
// Success resets the backoff state.
func (ep *ExecutorProvider) GetExecutor() (AgentExecutor, error) {
	ep.mu.Lock()
	defer ep.mu.Unlock()

	if ep.execInit {
		return ep.cachedExec, nil
	}

	if ep.execFailCount > 0 {
		backoff := time.Duration(1<<min(ep.execFailCount-1, 4)) * time.Second
		elapsed := ep.clock.Now().Sub(ep.execLastFailAt)
		if elapsed < backoff {
			return nil, fmt.Errorf("%w: retry cooldown (attempt %d, next retry in %v)",
				errExecutorInit, ep.execFailCount, backoff-elapsed)
		}
	}

	exec, err := ep.factory(ep.maestroDir, ep.watcherCfg, ep.logLevel)
	if err != nil {
		ep.execFailCount++
		ep.execLastFailAt = ep.clock.Now()
		return nil, fmt.Errorf("%w: %w", errExecutorInit, err)
	}

	ep.cachedExec = exec
	ep.execInit = true
	ep.execFailCount = 0
	return ep.cachedExec, nil
}

// SetFactory overrides the executor factory and resets all cached state.
// The old executor is closed outside the lock to prevent blocking GetExecutor() callers.
func (ep *ExecutorProvider) SetFactory(f ExecutorFactory) {
	ep.mu.Lock()
	old := ep.cachedExec
	ep.factory = f
	ep.cachedExec = nil
	ep.execInit = false
	ep.execFailCount = 0
	ep.execLastFailAt = time.Time{}
	ep.mu.Unlock()

	if old != nil {
		if err := old.Close(); err != nil {
			log.Printf("WARN: failed to close old executor: %v", err)
		}
	}
}

// Factory returns the current ExecutorFactory. Used by components that need
// the raw factory (e.g., reconcile.Engine which creates per-use executors).
func (ep *ExecutorProvider) Factory() ExecutorFactory {
	ep.mu.Lock()
	defer ep.mu.Unlock()
	return ep.factory
}

// CloseExecutor releases the shared executor's resources. Safe to call multiple times.
func (ep *ExecutorProvider) CloseExecutor() {
	ep.mu.Lock()
	exec := ep.cachedExec
	ep.cachedExec = nil
	ep.execInit = false
	ep.execFailCount = 0
	ep.execLastFailAt = time.Time{}
	ep.mu.Unlock()

	if exec != nil {
		if err := exec.Close(); err != nil {
			log.Printf("WARN: failed to close executor: %v", err)
		}
	}
}
