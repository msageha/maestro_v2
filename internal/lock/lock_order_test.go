//go:build lockorder

package lock

import (
	"os"
	"testing"
)

func init() {
	// Force panic mode for these tests so violations are detected.
	os.Setenv("MAESTRO_LOCKORDER", "panic")
}

func TestLevelForKey(t *testing.T) {
	tests := []struct {
		key       string
		wantLevel int
		wantOK    bool
	}{
		{"queue:worker1", 1, true},
		{"queue:planner", 1, true},
		{"state:cmd123", 2, true},
		{"state:abc", 2, true},
		{"result:planner", 3, true},
		{"result:xyz", 3, true},
		{"unknown:key", 0, false},
		{"noprefix", 0, false},
		{"", 0, false},
	}
	for _, tt := range tests {
		level, ok := LevelForKey(tt.key)
		if level != tt.wantLevel || ok != tt.wantOK {
			t.Errorf("LevelForKey(%q) = (%d, %v), want (%d, %v)",
				tt.key, level, ok, tt.wantLevel, tt.wantOK)
		}
	}
}

func TestLockOrder_ValidOrder(t *testing.T) {
	// queue → state → result is the canonical valid order.
	m := NewMutexMap()

	m.Lock("queue:worker1")
	m.Lock("state:cmd1")
	m.Lock("result:planner")

	m.Unlock("result:planner")
	m.Unlock("state:cmd1")
	m.Unlock("queue:worker1")
}

func TestLockOrder_ValidOrder_QueueThenState(t *testing.T) {
	m := NewMutexMap()

	m.Lock("queue:planner")
	m.Lock("state:cmd2")

	m.Unlock("state:cmd2")
	m.Unlock("queue:planner")
}

func TestLockOrder_ValidOrder_StateThenResult(t *testing.T) {
	m := NewMutexMap()

	m.Lock("state:cmd3")
	m.Lock("result:planner")

	m.Unlock("result:planner")
	m.Unlock("state:cmd3")
}

func TestLockOrder_ValidOrder_SingleLock(t *testing.T) {
	m := NewMutexMap()

	m.Lock("state:cmd4")
	m.Unlock("state:cmd4")

	m.Lock("queue:w1")
	m.Unlock("queue:w1")

	m.Lock("result:p")
	m.Unlock("result:p")
}

func TestLockOrder_ValidOrder_SameLevelDifferentKeys(t *testing.T) {
	// Same-level locks (e.g. two queue: locks) are allowed.
	m := NewMutexMap()

	m.Lock("queue:worker1")
	m.Lock("queue:worker2")

	m.Unlock("queue:worker2")
	m.Unlock("queue:worker1")
}

func TestLockOrder_ValidOrder_UnknownPrefix(t *testing.T) {
	// Keys without known prefix are not tracked — no panic.
	m := NewMutexMap()

	m.Lock("unknown:key1")
	m.Lock("queue:w1")
	m.Lock("state:cmd1")

	m.Unlock("state:cmd1")
	m.Unlock("queue:w1")
	m.Unlock("unknown:key1")
}

func TestLockOrder_Violation_StateBeforeQueue(t *testing.T) {
	m := NewMutexMap()
	m.Lock("state:cmd1")

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for lock order violation: queue after state")
		}
		m.Unlock("state:cmd1")
	}()

	m.Lock("queue:worker1") // violation: level 1 while holding level 2
}

func TestLockOrder_Violation_ResultBeforeState(t *testing.T) {
	m := NewMutexMap()
	m.Lock("result:planner")

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for lock order violation: state after result")
		}
		m.Unlock("result:planner")
	}()

	m.Lock("state:cmd1") // violation: level 2 while holding level 3
}

func TestLockOrder_Violation_ResultBeforeQueue(t *testing.T) {
	m := NewMutexMap()
	m.Lock("result:planner")

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for lock order violation: queue after result")
		}
		m.Unlock("result:planner")
	}()

	m.Lock("queue:worker1") // violation: level 1 while holding level 3
}

func TestLockOrder_ReleaseAndReacquire(t *testing.T) {
	// After releasing a higher-level lock, lower-level is fine.
	m := NewMutexMap()

	m.Lock("state:cmd1")
	m.Unlock("state:cmd1")

	// state is released, so queue is allowed now.
	m.Lock("queue:worker1")
	m.Unlock("queue:worker1")
}

func TestParseLockOrderMode(t *testing.T) {
	tests := []struct {
		input string
		want  lockOrderMode
	}{
		{"off", lockOrderOff},
		{"", lockOrderOff},
		{"warn", lockOrderWarn},
		{"1", lockOrderWarn},
		{"true", lockOrderWarn},
		{"panic", lockOrderPanic},
		{"strict", lockOrderPanic},
		{"2", lockOrderPanic},
		{"WARN", lockOrderWarn},
		{"PANIC", lockOrderPanic},
	}
	for _, tt := range tests {
		got := parseLockOrderMode(tt.input)
		if got != tt.want {
			t.Errorf("parseLockOrderMode(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestCurrentGID(t *testing.T) {
	gid := currentGID()
	if gid == 0 {
		t.Error("currentGID() returned 0, expected a positive goroutine ID")
	}
}
