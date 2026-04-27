// Package clock holds the canonical Clock abstraction used across the
// codebase. Centralising the interface here lets test packages and feature
// packages (daemon/core, metrics, testutil, …) share a single contract
// without importing each other.
//
// F-038: prior to this package, three independent declarations existed
// (core.Clock, metrics.Clock, testutil.Clock). They were structurally
// identical but required adapter shims at every boundary. Use clock.Clock
// for production wiring and embed it when extra methods are needed (see
// testutil.Clock for the Since() extension).
package clock

import "time"

// Clock abstracts time.Now() so wall-clock-dependent code can be tested
// deterministically. Implementations must be safe for concurrent use.
type Clock interface {
	Now() time.Time
}

// RealClock is the production implementation that delegates to time.Now().
type RealClock struct{}

// Now returns the current wall-clock time.
func (RealClock) Now() time.Time { return time.Now() }
