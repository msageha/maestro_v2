package daemon

import (
	"fmt"
	"sync"
	"time"

	"github.com/msageha/maestro_v2/internal/model"
)

// ErrorPolicy controls how ExecutorProvider handles initialization failures.
type ErrorPolicy int

const (
	// PolicyRetryBackoff retries with exponential backoff (1s→2s→4s→8s→16s cap).
	// Used by Dispatcher where transient failures are expected.
	PolicyRetryBackoff ErrorPolicy = iota

	// PolicyCacheError caches the first initialization error permanently.
	// Callers must call SetFactory to reset. Used by ResultHandler/CancelHandler
	// where the executor is expected to succeed on first attempt.
	PolicyCacheError
)

// ExecutorProvider encapsulates lazy initialization, caching, and lifecycle
// management of a shared AgentExecutor instance. It replaces the duplicated
// executor cache patterns in Dispatcher, ResultHandler, and CancelHandler.
type ExecutorProvider struct {
	maestroDir string
	watcherCfg model.WatcherConfig
	logLevel   string
	clock      Clock
	policy     ErrorPolicy

	mu              sync.Mutex
	factory         ExecutorFactory
	cachedExec      AgentExecutor
	execInit        bool
	cachedExecErr   error     // used only by PolicyCacheError
	execFailCount   int       // used only by PolicyRetryBackoff
	execLastFailAt  time.Time // used only by PolicyRetryBackoff
}

// NewExecutorProvider creates an ExecutorProvider.
func NewExecutorProvider(
	maestroDir string,
	watcherCfg model.WatcherConfig,
	logLevel string,
	factory ExecutorFactory,
	clock Clock,
	policy ErrorPolicy,
) *ExecutorProvider {
	return &ExecutorProvider{
		maestroDir: maestroDir,
		watcherCfg: watcherCfg,
		logLevel:   logLevel,
		factory:    factory,
		clock:      clock,
		policy:     policy,
	}
}

// GetExecutor returns the shared executor instance, creating it lazily on first call.
//
// PolicyRetryBackoff: On failure, subsequent calls retry with exponential backoff
// (1s→2s→4s→8s→16s cap). Success resets the backoff state.
//
// PolicyCacheError: On failure, the error is cached permanently. Subsequent calls
// return the cached error without retrying. Use SetFactory to reset.
func (ep *ExecutorProvider) GetExecutor() (AgentExecutor, error) {
	ep.mu.Lock()
	defer ep.mu.Unlock()

	if ep.execInit {
		if ep.policy == PolicyCacheError && ep.cachedExecErr != nil {
			return nil, fmt.Errorf("%w: %v", errExecutorInit, ep.cachedExecErr)
		}
		return ep.cachedExec, nil
	}

	if ep.policy == PolicyRetryBackoff && ep.execFailCount > 0 {
		backoff := time.Duration(1<<min(ep.execFailCount-1, 4)) * time.Second
		elapsed := ep.clock.Now().Sub(ep.execLastFailAt)
		if elapsed < backoff {
			return nil, fmt.Errorf("%w: retry cooldown (attempt %d, next retry in %v)",
				errExecutorInit, ep.execFailCount, backoff-elapsed)
		}
	}

	exec, err := ep.factory(ep.maestroDir, ep.watcherCfg, ep.logLevel)
	if err != nil {
		switch ep.policy {
		case PolicyRetryBackoff:
			ep.execFailCount++
			ep.execLastFailAt = ep.clock.Now()
		case PolicyCacheError:
			ep.cachedExecErr = err
			ep.execInit = true
		}
		return nil, fmt.Errorf("%w: %v", errExecutorInit, err)
	}

	ep.cachedExec = exec
	ep.execInit = true
	ep.execFailCount = 0
	return ep.cachedExec, nil
}

// SetFactory overrides the executor factory and resets all cached state.
// The old executor is closed while holding the lock to prevent races.
func (ep *ExecutorProvider) SetFactory(f ExecutorFactory) {
	ep.mu.Lock()
	old := ep.cachedExec
	ep.factory = f
	ep.cachedExec = nil
	ep.cachedExecErr = nil
	ep.execInit = false
	ep.execFailCount = 0
	ep.execLastFailAt = time.Time{}
	if old != nil {
		_ = old.Close()
	}
	ep.mu.Unlock()
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
	ep.cachedExecErr = nil
	ep.execInit = false
	ep.execFailCount = 0
	ep.execLastFailAt = time.Time{}
	ep.mu.Unlock()

	if exec != nil {
		_ = exec.Close()
	}
}
