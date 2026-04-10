package reconcile

// RepairPatternID identifies which reconciliation pattern produced a repair.
type RepairPatternID string

const (
	// PatternR0 identifies repairs produced by the R0PlanningStuck reconciler.
	PatternR0         RepairPatternID = "R0"
	PatternR0Dispatch RepairPatternID = "R0-dispatch"
	PatternR0b        RepairPatternID = "R0b"
	PatternR1         RepairPatternID = "R1"
	PatternR2         RepairPatternID = "R2"
	PatternR3         RepairPatternID = "R3"
	PatternR4         RepairPatternID = "R4"
	PatternR5         RepairPatternID = "R5"
	PatternR6         RepairPatternID = "R6"
)

// NotificationKind identifies the type of deferred Planner notification.
type NotificationKind string

const (
	// NotifyReFill requests the planner to re-fill a phase's worker slots.
	NotifyReFill      NotificationKind = "re_fill"
	NotifyReEvaluate  NotificationKind = "re_evaluate"
	NotifyFillTimeout NotificationKind = "fill_timeout"
)

// Repair describes a single repair action performed by a reconciliation pattern.
type Repair struct {
	Pattern   RepairPatternID // which pattern produced this repair
	CommandID string
	TaskID    string
	Detail    string
}

// DeferredNotification captures a Planner notification to execute outside scanMu.Lock.
type DeferredNotification struct {
	Kind           NotificationKind // notification type
	CommandID      string           // target command
	Reason         string           // human-readable reason (for re_evaluate)
	TimedOutPhases map[string]bool  // phase names (for fill_timeout)
}

// Outcome is the result of a single pattern's Apply call.
type Outcome struct {
	Repairs       []Repair
	Notifications []DeferredNotification
}

// Pattern is the Strategy interface for individual reconciliation patterns.
type Pattern interface {
	Apply(run *Run) Outcome
}
