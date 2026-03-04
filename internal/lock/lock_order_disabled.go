//go:build !lockorder

package lock

// noopOrderChecker is compiled in production builds (without the lockorder
// build tag).  All methods are empty and inlined by the compiler, resulting
// in zero runtime overhead.
type noopOrderChecker struct{}

func newOrderChecker() orderChecker          { return noopOrderChecker{} }
func (noopOrderChecker) BeforeLock(string)   {}
func (noopOrderChecker) AfterLock(string)    {}
func (noopOrderChecker) BeforeUnlock(string) {}
