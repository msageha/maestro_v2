package daemon

import (
	"log"
	"sync/atomic"
	"testing"

	"github.com/msageha/maestro_v2/internal/events"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
)

// newTestAPIContext builds a minimal apiContext for unit testing.
// Late-bound fields (eventBus, fileLockHolder) are left nil by default
// and can be wired via the respective setters.
func newTestAPIContext(t *testing.T) *apiContext {
	t.Helper()
	return &apiContext{
		maestroDir: t.TempDir(),
		config:     &model.Config{},
		clock:      RealClock{},
		lockMap:    lock.NewMutexMap(),
		logFn:      func(level LogLevel, format string, args ...any) {},
		logger:     log.New(&discardWriter{}, "", 0),
		logLevel:   LogLevelError,
		selfWrites: newSelfWriteTracker(),
	}
}

func TestApiContext_SetEventBus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		busFn   func() *events.Bus
		wantNil bool
	}{
		{
			name:    "nil accessor returns nil bus",
			busFn:   nil,
			wantNil: true,
		},
		{
			name:    "accessor returning nil returns nil bus",
			busFn:   func() *events.Bus { return nil },
			wantNil: true,
		},
		{
			name: "accessor returning bus returns non-nil",
			busFn: func() *events.Bus {
				// Return a non-nil pointer (not started, but sufficient for the test)
				return &events.Bus{}
			},
			wantNil: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ac := newTestAPIContext(t)
			if tt.busFn != nil {
				ac.SetEventBus(tt.busFn)
			}
			got := ac.getEventBus()
			if tt.wantNil && got != nil {
				t.Errorf("getEventBus() = %v, want nil", got)
			}
			if !tt.wantNil && got == nil {
				t.Error("getEventBus() = nil, want non-nil")
			}
		})
	}
}

// mockFileLockHolder records lock/unlock calls for testing.
type mockFileLockHolder struct {
	lockCount   atomic.Int64
	unlockCount atomic.Int64
}

func (m *mockFileLockHolder) LockFiles()   { m.lockCount.Add(1) }
func (m *mockFileLockHolder) UnlockFiles() { m.unlockCount.Add(1) }

func TestApiContext_AcquireReleaseFileLock(t *testing.T) {
	t.Parallel()

	t.Run("nil holder skips lock without panic", func(t *testing.T) {
		t.Parallel()
		ac := newTestAPIContext(t)
		// fileLockHolder is nil by default; should not panic.
		ac.acquireFileLock()
		ac.releaseFileLock()
	})

	t.Run("wired holder delegates lock/unlock", func(t *testing.T) {
		t.Parallel()
		ac := newTestAPIContext(t)
		mock := &mockFileLockHolder{}
		ac.SetFileLockHolder(mock)

		ac.acquireFileLock()
		ac.releaseFileLock()

		if mock.lockCount.Load() != 1 {
			t.Errorf("lockCount = %d, want 1", mock.lockCount.Load())
		}
		if mock.unlockCount.Load() != 1 {
			t.Errorf("unlockCount = %d, want 1", mock.unlockCount.Load())
		}
	})
}

func TestApiContext_NotifySelfWrite_NilBus(t *testing.T) {
	t.Parallel()
	ac := newTestAPIContext(t)
	// eventBus is nil; should not panic.
	ac.notifySelfWrite("/tmp/test.yaml", "task", map[string]string{"key": "value"})
}

func TestApiContext_PublishQueueWritten_NilBus(t *testing.T) {
	t.Parallel()
	ac := newTestAPIContext(t)
	// eventBus is nil; should not panic.
	ac.publishQueueWritten("test_source")
}

func TestApiContext_RecordSelfWrite(t *testing.T) {
	t.Parallel()
	ac := newTestAPIContext(t)
	// Should not panic with valid selfWrites tracker.
	ac.recordSelfWrite("/tmp/test.yaml", map[string]string{"key": "value"})
}
