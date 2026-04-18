package reconcile

// RepairPatternID identifies which reconciliation pattern produced a repair.
type RepairPatternID string

const (
	// PatternR0 identifies repairs produced by the R0PlanningStuck reconciler.
	PatternR0 RepairPatternID = "R0"
	// PatternR0Dispatch identifies repairs produced by the R0 dispatch variant reconciler.
	PatternR0Dispatch RepairPatternID = "R0-dispatch"
	// PatternR0b identifies repairs produced by the R0b reconciler.
	PatternR0b RepairPatternID = "R0b"
	// PatternR1 identifies repairs produced by the R1 reconciler.
	PatternR1 RepairPatternID = "R1"
	// PatternR2 identifies repairs produced by the R2 reconciler.
	PatternR2 RepairPatternID = "R2"
	// PatternR3 identifies repairs produced by the R3 reconciler.
	PatternR3 RepairPatternID = "R3"
	// PatternR4 identifies repairs produced by the R4 reconciler.
	PatternR4 RepairPatternID = "R4"
	// PatternR5 identifies repairs produced by the R5 reconciler.
	PatternR5 RepairPatternID = "R5"
	// PatternR6 identifies repairs produced by the R6 reconciler.
	PatternR6 RepairPatternID = "R6"
	// PatternR7 identifies repairs produced by the R7 merge conflict reconciler.
	PatternR7 RepairPatternID = "R7"
	// PatternR8 identifies repairs produced by the R8 publish failed reconciler.
	PatternR8 RepairPatternID = "R8"
)

// NotificationKind identifies the type of deferred Planner notification.
type NotificationKind string

const (
	// NotifyReFill requests the planner to re-fill a phase's worker slots.
	NotifyReFill NotificationKind = "re_fill"
	// NotifyReEvaluate requests the planner to re-evaluate task eligibility.
	NotifyReEvaluate NotificationKind = "re_evaluate"
	// NotifyFillTimeout signals that a phase fill deadline has expired.
	NotifyFillTimeout NotificationKind = "fill_timeout"
	// NotifyConflictResolution requests the planner to generate a __conflict_resolution task.
	NotifyConflictResolution NotificationKind = "conflict_resolution"
	// NotifyConflictEscalation signals the planner that conflict resolution attempts are exhausted.
	NotifyConflictEscalation NotificationKind = "conflict_escalation"
	// NotifyPublishQuarantined signals the planner that publish failures have
	// reached the quarantine threshold and operator intervention is required.
	NotifyPublishQuarantined NotificationKind = "publish_quarantined"
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
	Reason         string           // human-readable reason (for re_evaluate, conflict_escalation)
	TimedOutPhases map[string]bool  // phase names (for fill_timeout)
	WorkerID       string           // worker ID (for conflict_resolution, conflict_escalation)
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
