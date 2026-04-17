package model

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"time"
)

// IDType is the prefix used in generated IDs to identify their entity type.
type IDType string

const (
	// IDTypeCommand identifies a command-scoped ID.
	IDTypeCommand IDType = "cmd"
	// IDTypeTask identifies a task-scoped ID.
	IDTypeTask IDType = "task"
	// IDTypePhase identifies a phase-scoped ID.
	IDTypePhase IDType = "phase"
	// IDTypeNotification identifies a notification-scoped ID.
	IDTypeNotification IDType = "ntf"
	// IDTypeResult identifies a result-scoped ID.
	IDTypeResult IDType = "res"
	// IDTypeSkillCandidate identifies a skill candidate ID.
	IDTypeSkillCandidate IDType = "skc"
	// IDTypeDispatch identifies a dispatch-scoped ID.
	IDTypeDispatch IDType = "dsp"
)

var validIDTypes = map[IDType]bool{
	IDTypeCommand:        true,
	IDTypeTask:           true,
	IDTypePhase:          true,
	IDTypeNotification:   true,
	IDTypeResult:         true,
	IDTypeSkillCandidate: true,
	IDTypeDispatch:       true,
}

var idRegex = regexp.MustCompile(`^(cmd|task|phase|ntf|res|skc|dsp)_[0-9]{10}_[0-9a-f]{8}([0-9a-f]{8})?$`)

// GenerateID creates a new unique ID with the given type prefix.
func GenerateID(idType IDType) (string, error) {
	if !validIDTypes[idType] {
		return "", fmt.Errorf("invalid ID type: %s", idType)
	}

	timestamp := time.Now().Unix()
	randomBytes := make([]byte, 8)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", fmt.Errorf("failed to generate random bytes: %w", err)
	}
	hexStr := hex.EncodeToString(randomBytes)

	return fmt.Sprintf("%s_%010d_%s", idType, timestamp, hexStr), nil
}

// TaskIDCaller identifies the legitimate caller responsible for minting a
// task ID. Task ID generation must go through NewTaskID with one of the
// constants below so that callers are explicit and auditable. This is the
// single chokepoint that resolves the historical "task ID minted in two
// places (Planner vs Daemon queue_write)" Critical issue.
type TaskIDCaller string

const (
	// TaskIDCallerPlannerSubmit — Planner.plan submit (resolveNames).
	TaskIDCallerPlannerSubmit TaskIDCaller = "planner-submit"
	// TaskIDCallerPlannerSystemCommit — Planner.plan submit system_commit task.
	TaskIDCallerPlannerSystemCommit TaskIDCaller = "planner-system-commit"
	// TaskIDCallerPlannerRetry — Planner.plan retry-task.
	TaskIDCallerPlannerRetry TaskIDCaller = "planner-retry"
	// TaskIDCallerPlannerInject — Planner.plan add-task (ad-hoc task injection into sealed plan).
	TaskIDCallerPlannerInject TaskIDCaller = "planner-inject"
	// TaskIDCallerDaemonRetry — Daemon TaskRetryHandler (automatic retry).
	TaskIDCallerDaemonRetry TaskIDCaller = "daemon-retry-handler"
	// TaskIDCallerDaemonConflictResolution — Daemon R7 reconciler (conflict resolution dispatch).
	TaskIDCallerDaemonConflictResolution TaskIDCaller = "daemon-conflict-resolution"
	// TaskIDCallerSystemInternal — internal/test entrypoint for queue_write
	// task path. NOT exposed via the maestro CLI.
	TaskIDCallerSystemInternal TaskIDCaller = "system-internal"
)

var validTaskIDCallers = map[TaskIDCaller]bool{
	TaskIDCallerPlannerSubmit:            true,
	TaskIDCallerPlannerSystemCommit:      true,
	TaskIDCallerPlannerRetry:             true,
	TaskIDCallerPlannerInject:            true,
	TaskIDCallerDaemonRetry:              true,
	TaskIDCallerDaemonConflictResolution: true,
	TaskIDCallerSystemInternal:           true,
}

// NewTaskID is the single, audited entrypoint for minting task IDs. The
// caller argument MUST be one of the TaskIDCaller constants so the origin
// of every task ID is explicit and grep-able.
func NewTaskID(caller TaskIDCaller) (string, error) {
	if !validTaskIDCallers[caller] {
		return "", fmt.Errorf("unknown caller %q (must be one of TaskIDCaller constants)", caller)
	}
	return GenerateID(IDTypeTask)
}

// ValidateID reports whether id matches the expected ID format.
func ValidateID(id string) bool {
	return idRegex.MatchString(id)
}

// ParseIDType extracts the IDType prefix from a structured ID string.
func ParseIDType(id string) (IDType, error) {
	if !ValidateID(id) {
		return "", fmt.Errorf("invalid ID format: %s", id)
	}
	match := idRegex.FindStringSubmatch(id)
	return IDType(match[1]), nil
}
