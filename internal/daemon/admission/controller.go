// Package admission provides concurrency admission control for verify, repair,
// and rollout operations. It enforces per-operation-type slot limits so that
// the daemon does not exceed the configured concurrency ceilings.
package admission

import (
	"strings"
	"sync"

	"github.com/msageha/maestro_v2/internal/model"
)

// OpType classifies a task into one of the known operation categories.
type OpType int

const (
	// OpVerify represents verification tasks.
	OpVerify OpType = iota
	// OpRepair represents repair tasks.
	OpRepair
	// OpRollout represents rollout tasks.
	OpRollout
	// OpUnknown represents tasks that do not match any known category.
	OpUnknown
)

// String returns a human-readable name for the operation type.
func (o OpType) String() string {
	switch o {
	case OpVerify:
		return "verify"
	case OpRepair:
		return "repair"
	case OpRollout:
		return "rollout"
	case OpUnknown:
		return "unknown"
	default:
		return "unknown"
	}
}

// Controller enforces per-operation-type concurrency limits. Each operation
// type (verify, repair, rollout) has a maximum number of concurrent slots.
// OpUnknown tasks are always admitted without limit.
type Controller struct {
	maxVerify  int
	maxRepair  int
	maxRollout int
	slots      map[OpType]int
	mu         sync.Mutex
}

// NewController creates a Controller using the effective limits from the
// supplied AdmissionControl configuration.
func NewController(cfg model.AdmissionControl) *Controller {
	return &Controller{
		maxVerify:  cfg.EffectiveMaxConcurrentVerify(),
		maxRepair:  cfg.EffectiveMaxConcurrentRepair(),
		maxRollout: cfg.EffectiveMaxConcurrentRollout(),
		slots: map[OpType]int{
			OpVerify:  0,
			OpRepair:  0,
			OpRollout: 0,
		},
	}
}

// TryAcquire attempts to acquire a concurrency slot for the given operation
// type. It returns true if the slot was acquired, false if the limit has been
// reached. OpUnknown always succeeds (no limit enforced).
func (c *Controller) TryAcquire(op OpType) bool {
	if op == OpUnknown {
		return true
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	limit := c.maxFor(op)
	if c.slots[op] >= limit {
		return false
	}
	c.slots[op]++
	return true
}

// Release decrements the slot count for the given operation type. The count
// will not go below zero.
func (c *Controller) Release(op OpType) {
	if op == OpUnknown {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.slots[op] > 0 {
		c.slots[op]--
	}
}

// ClassifyTask determines the OpType for a task based on keyword matching
// against its Purpose field (case-insensitive).
func (c *Controller) ClassifyTask(task *model.Task) OpType {
	purpose := strings.ToLower(task.Purpose)
	switch {
	case strings.Contains(purpose, "verify"), strings.Contains(purpose, "verification"):
		return OpVerify
	case strings.Contains(purpose, "repair"):
		return OpRepair
	case strings.Contains(purpose, "rollout"):
		return OpRollout
	default:
		return OpUnknown
	}
}

// Reset clears all slot counts to zero.
func (c *Controller) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()

	for op := range c.slots {
		c.slots[op] = 0
	}
}

// RecordInFlight resets the slot counts and then classifies each provided task,
// incrementing the appropriate slot for each one.
func (c *Controller) RecordInFlight(tasks []*model.Task) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for op := range c.slots {
		c.slots[op] = 0
	}

	for _, t := range tasks {
		op := c.classifyTaskUnlocked(t)
		if op != OpUnknown {
			c.slots[op]++
		}
	}
}

// ActiveCount returns the current slot count for the given operation type.
func (c *Controller) ActiveCount(op OpType) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.slots[op]
}

// Snapshot returns a copy of the current slots map.
func (c *Controller) Snapshot() map[OpType]int {
	c.mu.Lock()
	defer c.mu.Unlock()

	snap := make(map[OpType]int, len(c.slots))
	for op, count := range c.slots {
		snap[op] = count
	}
	return snap
}

// maxFor returns the maximum concurrency limit for the given operation type.
func (c *Controller) maxFor(op OpType) int {
	switch op {
	case OpVerify:
		return c.maxVerify
	case OpRepair:
		return c.maxRepair
	case OpRollout:
		return c.maxRollout
	default:
		return 0
	}
}

// classifyTaskUnlocked performs classification without acquiring the mutex.
// Caller must hold c.mu.
func (c *Controller) classifyTaskUnlocked(task *model.Task) OpType {
	purpose := strings.ToLower(task.Purpose)
	switch {
	case strings.Contains(purpose, "verify"), strings.Contains(purpose, "verification"):
		return OpVerify
	case strings.Contains(purpose, "repair"):
		return OpRepair
	case strings.Contains(purpose, "rollout"):
		return OpRollout
	default:
		return OpUnknown
	}
}
