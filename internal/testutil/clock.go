package testutil

import (
	"sync"
	"time"
)

// Clock abstracts time.Now and time.Since for deterministic testing.
type Clock interface {
	Now() time.Time
	Since(time.Time) time.Duration
}

// FakeClock implements clock interfaces (core.Clock, testutil.Clock, etc.) for deterministic testing.
// All methods are safe for concurrent use.
type FakeClock struct {
	mu       sync.Mutex
	NowValue time.Time
}

// Now returns the current fake time.
func (f *FakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.NowValue
}

// Since returns the duration elapsed since t according to the fake clock.
func (f *FakeClock) Since(t time.Time) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.NowValue.Sub(t)
}

// Advance moves the fake clock forward by d.
func (f *FakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.NowValue = f.NowValue.Add(d)
}

// SetNow sets the fake clock to an arbitrary time.
func (f *FakeClock) SetNow(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.NowValue = t
}
