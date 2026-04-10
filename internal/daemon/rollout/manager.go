package rollout

import (
	"fmt"
	"sync"
	"time"
)

// SlotStatus represents the status of a rollout slot.
type SlotStatus string

const (
	// SlotPending indicates the slot has not yet started.
	SlotPending SlotStatus = "pending"
	// SlotRunning indicates the slot is actively executing.
	SlotRunning SlotStatus = "running"
	// SlotCompleted indicates the slot finished successfully.
	SlotCompleted SlotStatus = "completed"
	// SlotFailed indicates the slot encountered an error.
	SlotFailed SlotStatus = "failed"
)

// Slot represents a single rollout execution slot within a group.
type Slot struct {
	Index      int
	ID         string
	WorkerID   string
	BranchName string
	Status     SlotStatus
}

// GroupState represents the lifecycle state of a rollout group.
type GroupState string

const (
	// GroupPending indicates the group has not yet started.
	GroupPending GroupState = "pending"
	// GroupRunning indicates the group is actively executing slots.
	GroupRunning GroupState = "running"
	// GroupSelecting indicates the group is evaluating results to pick a winner.
	GroupSelecting GroupState = "selecting"
	// GroupCompleted indicates the group finished and a winner was selected.
	GroupCompleted GroupState = "completed"
	// GroupCancelled indicates the group was cancelled before completion.
	GroupCancelled GroupState = "cancelled"
)

// IsTerminal returns true if the group state is a terminal state.
func IsTerminal(s GroupState) bool {
	return s == GroupCompleted || s == GroupCancelled
}

// Group represents a multi-rollout group for a single task.
type Group struct {
	ID          string
	TaskID      string
	CommandID   string
	Slots       []Slot
	State       GroupState
	WinnerSlot  int
	CreatedAt   time.Time
	CompletedAt *time.Time
}

// Manager manages multi-rollout groups with thread-safe operations.
type Manager struct {
	mu          sync.Mutex
	groups      map[string]*Group
	maxParallel int
}

// NewManager creates a new rollout Manager with the given max parallel slot count.
func NewManager(maxParallel int) *Manager {
	return &Manager{
		groups:      make(map[string]*Group),
		maxParallel: maxParallel,
	}
}

// CreateGroup creates a new rollout group for the given task.
// Returns an error if a group already exists for the taskID or slotCount is invalid.
func (m *Manager) CreateGroup(taskID, commandID string, slotCount int) (*Group, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.groups[taskID]; exists {
		return nil, fmt.Errorf("group already exists for task %s", taskID)
	}
	if slotCount < 1 || slotCount > m.maxParallel {
		return nil, fmt.Errorf("slot count %d out of range [1, %d]", slotCount, m.maxParallel)
	}

	slots := make([]Slot, slotCount)
	for i := range slots {
		slots[i] = Slot{
			Index:  i,
			ID:     fmt.Sprintf("%s_slot_%d", taskID, i),
			Status: SlotPending,
		}
	}

	group := &Group{
		ID:         fmt.Sprintf("rollout_%s", taskID),
		TaskID:     taskID,
		CommandID:  commandID,
		Slots:      slots,
		State:      GroupPending,
		WinnerSlot: -1,
		CreatedAt:  time.Now(),
	}

	m.groups[taskID] = group
	return group, nil
}

// GetGroup returns the group for the given taskID.
func (m *Manager) GetGroup(taskID string) (*Group, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	g, ok := m.groups[taskID]
	return g, ok
}

// UpdateSlotStatus updates the status of a specific slot in a group.
func (m *Manager) UpdateSlotStatus(taskID string, slotIndex int, status SlotStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	g, ok := m.groups[taskID]
	if !ok {
		return fmt.Errorf("group not found for task %s", taskID)
	}
	if slotIndex < 0 || slotIndex >= len(g.Slots) {
		return fmt.Errorf("slot index %d out of range [0, %d)", slotIndex, len(g.Slots))
	}

	g.Slots[slotIndex].Status = status
	return nil
}

// IsGroupComplete returns true if all slots in the group have reached a terminal state.
func (m *Manager) IsGroupComplete(taskID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	g, ok := m.groups[taskID]
	if !ok {
		return false
	}

	for _, s := range g.Slots {
		if s.Status != SlotCompleted && s.Status != SlotFailed {
			return false
		}
	}
	return true
}

// SetWinner designates a winning slot for the group.
func (m *Manager) SetWinner(taskID string, slotIndex int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	g, ok := m.groups[taskID]
	if !ok {
		return fmt.Errorf("group not found for task %s", taskID)
	}
	if slotIndex < 0 || slotIndex >= len(g.Slots) {
		return fmt.Errorf("slot index %d out of range [0, %d)", slotIndex, len(g.Slots))
	}

	g.WinnerSlot = slotIndex
	g.State = GroupCompleted
	now := time.Now()
	g.CompletedAt = &now
	return nil
}

// CancelGroup cancels a group and all its non-terminal slots.
func (m *Manager) CancelGroup(taskID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	g, ok := m.groups[taskID]
	if !ok {
		return fmt.Errorf("group not found for task %s", taskID)
	}

	g.State = GroupCancelled
	now := time.Now()
	g.CompletedAt = &now

	return nil
}

// RemoveGroup removes a group from the manager.
func (m *Manager) RemoveGroup(taskID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.groups, taskID)
}

// ActiveGroupCount returns the number of non-terminal groups.
func (m *Manager) ActiveGroupCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	count := 0
	for _, g := range m.groups {
		if !IsTerminal(g.State) {
			count++
		}
	}
	return count
}

// ListGroups returns all groups managed by this manager.
func (m *Manager) ListGroups() []*Group {
	m.mu.Lock()
	defer m.mu.Unlock()

	groups := make([]*Group, 0, len(m.groups))
	for _, g := range m.groups {
		groups = append(groups, g)
	}
	return groups
}
