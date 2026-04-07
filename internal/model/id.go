package model

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"time"
)

type IDType string

const (
	IDTypeCommand      IDType = "cmd"
	IDTypeTask         IDType = "task"
	IDTypePhase        IDType = "phase"
	IDTypeNotification IDType = "ntf"
	IDTypeResult         IDType = "res"
	IDTypeSkillCandidate IDType = "skc"
)

var validIDTypes = map[IDType]bool{
	IDTypeCommand:        true,
	IDTypeTask:           true,
	IDTypePhase:          true,
	IDTypeNotification:   true,
	IDTypeResult:         true,
	IDTypeSkillCandidate: true,
}

var idRegex = regexp.MustCompile(`^(cmd|task|phase|ntf|res|skc)_[0-9]{10}_[0-9a-f]{8}$`)

func GenerateID(idType IDType) (string, error) {
	if !validIDTypes[idType] {
		return "", fmt.Errorf("invalid ID type: %s", idType)
	}

	timestamp := time.Now().Unix()
	randomBytes := make([]byte, 4)
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
	// TaskIDCallerDaemonRetry — Daemon TaskRetryHandler (automatic retry).
	TaskIDCallerDaemonRetry TaskIDCaller = "daemon-retry-handler"
	// TaskIDCallerSystemInternal — internal/test entrypoint for queue_write
	// task path. NOT exposed via the maestro CLI.
	TaskIDCallerSystemInternal TaskIDCaller = "system-internal"
)

var validTaskIDCallers = map[TaskIDCaller]bool{
	TaskIDCallerPlannerSubmit:       true,
	TaskIDCallerPlannerSystemCommit: true,
	TaskIDCallerPlannerRetry:        true,
	TaskIDCallerDaemonRetry:         true,
	TaskIDCallerSystemInternal:      true,
}

// NewTaskID is the single, audited entrypoint for minting task IDs. The
// caller argument MUST be one of the TaskIDCaller constants so the origin
// of every task ID is explicit and grep-able.
func NewTaskID(caller TaskIDCaller) (string, error) {
	if !validTaskIDCallers[caller] {
		return "", fmt.Errorf("NewTaskID: unknown caller %q (must be one of TaskIDCaller constants)", caller)
	}
	return GenerateID(IDTypeTask)
}

func ValidateID(id string) bool {
	return idRegex.MatchString(id)
}

func ParseIDType(id string) (IDType, error) {
	if !ValidateID(id) {
		return "", fmt.Errorf("invalid ID format: %s", id)
	}
	match := idRegex.FindStringSubmatch(id)
	return IDType(match[1]), nil
}
