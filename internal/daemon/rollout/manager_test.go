package rollout

import (
	"sync"
	"testing"
)

func TestCreateGroup_Normal(t *testing.T) {
	m := NewManager(5)

	g, err := m.CreateGroup("task1", "cmd1", 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if g.TaskID != "task1" {
		t.Errorf("TaskID = %q, want %q", g.TaskID, "task1")
	}
	if g.CommandID != "cmd1" {
		t.Errorf("CommandID = %q, want %q", g.CommandID, "cmd1")
	}
	if len(g.Slots) != 3 {
		t.Fatalf("len(Slots) = %d, want 3", len(g.Slots))
	}
	if g.State != GroupPending {
		t.Errorf("State = %q, want %q", g.State, GroupPending)
	}
	if g.WinnerSlot != -1 {
		t.Errorf("WinnerSlot = %d, want -1", g.WinnerSlot)
	}

	for i, s := range g.Slots {
		if s.Index != i {
			t.Errorf("Slot[%d].Index = %d, want %d", i, s.Index, i)
		}
		if s.Status != SlotPending {
			t.Errorf("Slot[%d].Status = %q, want %q", i, s.Status, SlotPending)
		}
		expectedID := "task1_slot_" + itoa(i)
		if s.ID != expectedID {
			t.Errorf("Slot[%d].ID = %q, want %q", i, s.ID, expectedID)
		}
	}
}

func TestCreateGroup_DuplicateTaskID(t *testing.T) {
	m := NewManager(5)

	_, err := m.CreateGroup("task1", "cmd1", 2)
	if err != nil {
		t.Fatalf("unexpected error on first create: %v", err)
	}

	_, err = m.CreateGroup("task1", "cmd2", 2)
	if err == nil {
		t.Error("expected error on duplicate taskID, got nil")
	}
}

func TestCreateGroup_InvalidSlotCount(t *testing.T) {
	m := NewManager(3)

	_, err := m.CreateGroup("task1", "cmd1", 0)
	if err == nil {
		t.Error("expected error for slotCount 0")
	}

	_, err = m.CreateGroup("task2", "cmd1", 4)
	if err == nil {
		t.Error("expected error for slotCount > maxParallel")
	}
}

func TestUpdateSlotStatus_AndIsGroupComplete(t *testing.T) {
	m := NewManager(5)
	_, _ = m.CreateGroup("task1", "cmd1", 3)

	if m.IsGroupComplete("task1") {
		t.Error("group should not be complete initially")
	}

	_ = m.UpdateSlotStatus("task1", 0, SlotCompleted)
	_ = m.UpdateSlotStatus("task1", 1, SlotRunning)
	_ = m.UpdateSlotStatus("task1", 2, SlotCompleted)

	if m.IsGroupComplete("task1") {
		t.Error("group should not be complete with a running slot")
	}

	_ = m.UpdateSlotStatus("task1", 1, SlotCompleted)
	if !m.IsGroupComplete("task1") {
		t.Error("group should be complete when all slots are completed")
	}
}

func TestIsGroupComplete_MixedTerminal(t *testing.T) {
	m := NewManager(5)
	_, _ = m.CreateGroup("task1", "cmd1", 3)

	_ = m.UpdateSlotStatus("task1", 0, SlotFailed)
	_ = m.UpdateSlotStatus("task1", 1, SlotCompleted)
	_ = m.UpdateSlotStatus("task1", 2, SlotCompleted)

	if !m.IsGroupComplete("task1") {
		t.Error("group should be complete with mixed failed+completed slots")
	}
}

func TestIsGroupComplete_NonExistent(t *testing.T) {
	m := NewManager(5)
	if m.IsGroupComplete("nonexistent") {
		t.Error("non-existent group should return false")
	}
}

func TestSetWinner(t *testing.T) {
	m := NewManager(5)
	_, _ = m.CreateGroup("task1", "cmd1", 3)

	err := m.SetWinner("task1", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	g, _ := m.GetGroup("task1")
	if g.WinnerSlot != 1 {
		t.Errorf("WinnerSlot = %d, want 1", g.WinnerSlot)
	}
	if g.State != GroupCompleted {
		t.Errorf("State = %q, want %q", g.State, GroupCompleted)
	}
	if g.CompletedAt == nil {
		t.Error("CompletedAt should be set")
	}
}

func TestSetWinner_InvalidIndex(t *testing.T) {
	m := NewManager(5)
	_, _ = m.CreateGroup("task1", "cmd1", 2)

	err := m.SetWinner("task1", 5)
	if err == nil {
		t.Error("expected error for invalid slot index")
	}
}

func TestSetWinner_NonExistentGroup(t *testing.T) {
	m := NewManager(5)
	err := m.SetWinner("nonexistent", 0)
	if err == nil {
		t.Error("expected error for non-existent group")
	}
}

func TestCancelGroup(t *testing.T) {
	m := NewManager(5)
	_, _ = m.CreateGroup("task1", "cmd1", 3)

	err := m.CancelGroup("task1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	g, _ := m.GetGroup("task1")
	if g.State != GroupCancelled {
		t.Errorf("State = %q, want %q", g.State, GroupCancelled)
	}
	if g.CompletedAt == nil {
		t.Error("CompletedAt should be set after cancel")
	}
}

func TestCancelGroup_NonExistent(t *testing.T) {
	m := NewManager(5)
	err := m.CancelGroup("nonexistent")
	if err == nil {
		t.Error("expected error for non-existent group")
	}
}

func TestActiveGroupCount(t *testing.T) {
	m := NewManager(5)

	if m.ActiveGroupCount() != 0 {
		t.Errorf("ActiveGroupCount = %d, want 0", m.ActiveGroupCount())
	}

	_, _ = m.CreateGroup("task1", "cmd1", 2)
	_, _ = m.CreateGroup("task2", "cmd2", 2)

	if m.ActiveGroupCount() != 2 {
		t.Errorf("ActiveGroupCount = %d, want 2", m.ActiveGroupCount())
	}

	_ = m.SetWinner("task1", 0)
	if m.ActiveGroupCount() != 1 {
		t.Errorf("ActiveGroupCount = %d, want 1", m.ActiveGroupCount())
	}

	_ = m.CancelGroup("task2")
	if m.ActiveGroupCount() != 0 {
		t.Errorf("ActiveGroupCount = %d, want 0", m.ActiveGroupCount())
	}
}

func TestRemoveGroup(t *testing.T) {
	m := NewManager(5)
	_, _ = m.CreateGroup("task1", "cmd1", 2)

	m.RemoveGroup("task1")

	_, ok := m.GetGroup("task1")
	if ok {
		t.Error("group should not exist after removal")
	}

	// Removing non-existent group should not panic
	m.RemoveGroup("nonexistent")
}

func TestListGroups(t *testing.T) {
	m := NewManager(5)
	_, _ = m.CreateGroup("task1", "cmd1", 2)
	_, _ = m.CreateGroup("task2", "cmd2", 3)

	groups := m.ListGroups()
	if len(groups) != 2 {
		t.Errorf("len(ListGroups) = %d, want 2", len(groups))
	}
}

func TestUpdateSlotStatus_Errors(t *testing.T) {
	m := NewManager(5)

	err := m.UpdateSlotStatus("nonexistent", 0, SlotRunning)
	if err == nil {
		t.Error("expected error for non-existent group")
	}

	_, _ = m.CreateGroup("task1", "cmd1", 2)
	err = m.UpdateSlotStatus("task1", -1, SlotRunning)
	if err == nil {
		t.Error("expected error for negative slot index")
	}
	err = m.UpdateSlotStatus("task1", 5, SlotRunning)
	if err == nil {
		t.Error("expected error for out-of-range slot index")
	}
}

func TestIsTerminal(t *testing.T) {
	tests := []struct {
		state    GroupState
		terminal bool
	}{
		{GroupPending, false},
		{GroupRunning, false},
		{GroupSelecting, false},
		{GroupCompleted, true},
		{GroupCancelled, true},
	}

	for _, tt := range tests {
		if got := IsTerminal(tt.state); got != tt.terminal {
			t.Errorf("IsTerminal(%q) = %v, want %v", tt.state, got, tt.terminal)
		}
	}
}

func TestConcurrentAccess(t *testing.T) {
	m := NewManager(10)
	const taskCount = 20
	const goroutineCount = 50

	// Create groups concurrently
	var wg sync.WaitGroup
	errs := make(chan error, goroutineCount*2)

	for i := 0; i < taskCount; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			taskID := "task_" + itoa(idx)
			_, err := m.CreateGroup(taskID, "cmd1", 3)
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()

	// Verify no unexpected errors (duplicates are expected if same idx runs twice, but we use unique indices)
	close(errs)
	for err := range errs {
		t.Errorf("unexpected error during concurrent create: %v", err)
	}

	// Concurrent updates on the same group
	errs2 := make(chan error, goroutineCount)
	for i := 0; i < goroutineCount; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			slotIdx := idx % 3
			status := SlotRunning
			if idx%2 == 0 {
				status = SlotCompleted
			}
			if err := m.UpdateSlotStatus("task_0", slotIdx, status); err != nil {
				errs2 <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs2)
	for err := range errs2 {
		t.Errorf("unexpected error during concurrent update: %v", err)
	}

	// Concurrent reads
	for i := 0; i < goroutineCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.GetGroup("task_0")
			m.IsGroupComplete("task_0")
			m.ActiveGroupCount()
			m.ListGroups()
		}()
	}
	wg.Wait()
}

// itoa converts an int to string without importing strconv.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	buf := make([]byte, 0, 10)
	for n > 0 {
		buf = append(buf, byte('0'+n%10))
		n /= 10
	}
	if neg {
		buf = append(buf, '-')
	}
	// reverse
	for i, j := 0, len(buf)-1; i < j; i, j = i+1, j-1 {
		buf[i], buf[j] = buf[j], buf[i]
	}
	return string(buf)
}
