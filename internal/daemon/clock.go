package daemon

import "time"

// Clock abstracts time.Now() for deterministic testing.
type Clock interface {
	Now() time.Time
}

// RealClock is the production Clock that delegates to time.Now().
type RealClock struct{}

// Now returns the current time.
func (RealClock) Now() time.Time { return time.Now() }
