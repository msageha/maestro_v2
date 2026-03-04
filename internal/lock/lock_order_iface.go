package lock

// orderChecker is the internal interface for lock-order verification.
// The active implementation is selected at build time via the "lockorder"
// build tag.  Without the tag a zero-overhead no-op is compiled in.
type orderChecker interface {
	BeforeLock(key string)   // may panic in strict mode
	AfterLock(key string)    // record held lock
	BeforeUnlock(key string) // must never panic
}
