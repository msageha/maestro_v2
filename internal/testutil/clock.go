package testutil

import "time"

// FakeClock implements clock interfaces (core.Clock, etc.) for deterministic testing.
type FakeClock struct {
	NowValue time.Time
}

func (f *FakeClock) Now() time.Time { return f.NowValue }
