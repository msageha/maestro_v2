package reconcile

// Repair describes a single repair action performed by a reconciliation pattern.
type Repair struct {
	Pattern   string // "R0", "R0b", "R1", "R2", "R3", "R4", "R5", "R6"
	CommandID string
	TaskID    string
	Detail    string
}

// DeferredNotification captures a Planner notification to execute outside scanMu.Lock.
type DeferredNotification struct {
	Kind           string          // "re_fill", "re_evaluate", "fill_timeout"
	CommandID      string          // target command
	Reason         string          // human-readable reason (for re_evaluate)
	TimedOutPhases map[string]bool // phase names (for fill_timeout)
}

// Outcome is the result of a single pattern's Apply call.
type Outcome struct {
	Repairs       []Repair
	Notifications []DeferredNotification
}

// Pattern is the Strategy interface for individual reconciliation patterns.
type Pattern interface {
	Name() string
	Apply(run *Run) Outcome
}
