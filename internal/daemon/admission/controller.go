// Package admission provides concurrency admission control for verify, repair,
// and rollout operations. It enforces per-operation-type slot limits so that
// the daemon does not exceed the configured concurrency ceilings.
package admission

import (
	"log"
	"strings"
	"sync"

	"github.com/msageha/maestro_v2/internal/model"
)

// defaultSaturationThreshold is the number of consecutive rejections for the
// same OpType that triggers a persistent saturation warning log.
const defaultSaturationThreshold = 5

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
//
// Note: There is no global "enabled" flag. The controller is always active
// when instantiated; disable by not calling TryAcquire (i.e., bypass at the
// call site).
type Controller struct {
	maxVerify           int
	maxRepair           int
	maxRollout          int
	slots               map[OpType]int
	consecutiveRejects  map[OpType]int
	saturationThreshold int
	logger              *log.Logger
	mu                  sync.Mutex
}

// NewController creates a Controller using the effective limits from the
// supplied AdmissionControl configuration.
func NewController(cfg model.AdmissionControl, opts ...ControllerOption) *Controller {
	c := &Controller{
		maxVerify:           cfg.EffectiveMaxConcurrentVerify(),
		maxRepair:           cfg.EffectiveMaxConcurrentRepair(),
		maxRollout:          cfg.EffectiveMaxConcurrentRollout(),
		saturationThreshold: defaultSaturationThreshold,
		slots: map[OpType]int{
			OpVerify:  0,
			OpRepair:  0,
			OpRollout: 0,
		},
		consecutiveRejects: map[OpType]int{
			OpVerify:  0,
			OpRepair:  0,
			OpRollout: 0,
		},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// ControllerOption configures a Controller.
type ControllerOption func(*Controller)

// WithLogger sets the logger for saturation warnings.
func WithLogger(l *log.Logger) ControllerOption {
	return func(c *Controller) { c.logger = l }
}

// WithSaturationThreshold sets the consecutive rejection count that triggers a warning.
func WithSaturationThreshold(n int) ControllerOption {
	return func(c *Controller) { c.saturationThreshold = n }
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
		c.consecutiveRejects[op]++
		if c.saturationThreshold > 0 && c.consecutiveRejects[op] == c.saturationThreshold {
			if c.logger != nil {
				c.logger.Printf("[WARN] admission_persistent_saturation op=%s consecutive_rejects=%d limit=%d active=%d",
					op, c.consecutiveRejects[op], limit, c.slots[op])
			}
		}
		return false
	}
	c.consecutiveRejects[op] = 0
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
	return c.classifyTaskUnlocked(task)
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
// This method is stateless (reads only the task argument) and is safe to
// call with or without holding c.mu.
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
