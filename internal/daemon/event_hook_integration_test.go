package daemon

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/msageha/maestro_v2/internal/agent"
	"github.com/msageha/maestro_v2/internal/events"
	"github.com/msageha/maestro_v2/internal/model"
)

// TestEventHookIntegration verifies that event hooks are published correctly
// and do not impact main processing flow.
func TestEventHookIntegration(t *testing.T) {
	tmpDir := t.TempDir()
	maestroDir := filepath.Join(tmpDir, ".maestro")

	if err := os.MkdirAll(filepath.Join(maestroDir, "queue"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(maestroDir, "results"), 0755); err != nil {
		t.Fatal(err)
	}

	bus := events.NewBus(100)
	defer bus.Close()

	var mu sync.Mutex
	taskStartedEvents := []events.Event{}
	taskCompletedEvents := []events.Event{}
	phaseTransitionEvents := []events.Event{}

	// Subscribe to all event types
	unsub1 := bus.Subscribe(events.EventTaskStarted, func(e events.Event) {
		mu.Lock()
		taskStartedEvents = append(taskStartedEvents, e)
		mu.Unlock()
	})
	defer unsub1()

	unsub2 := bus.Subscribe(events.EventTaskCompleted, func(e events.Event) {
		mu.Lock()
		taskCompletedEvents = append(taskCompletedEvents, e)
		mu.Unlock()
	})
	defer unsub2()

	unsub3 := bus.Subscribe(events.EventPhaseTransition, func(e events.Event) {
		mu.Lock()
		phaseTransitionEvents = append(phaseTransitionEvents, e)
		mu.Unlock()
	})
	defer unsub3()

	// Test Dispatcher event publishing
	t.Run("DispatcherPublishesTaskStartedEvent", func(t *testing.T) {
		cfg := model.Config{
			Queue: model.QueueConfig{
				PriorityAgingSec: 60,
			},
			Watcher: model.WatcherConfig{},
			Logging: model.LoggingConfig{Level: "info"},
		}

		logger := log.New(&bytes.Buffer{}, "", 0)
		dispatcher := NewDispatcher(maestroDir, cfg, nil, logger, LogLevelInfo)
		dispatcher.SetEventBus(bus)

		// Use mock executor
		mockExec := &MockExecutor{
			executeResult: agent.ExecResult{Error: nil, Retryable: false},
		}
		dispatcher.SetExecutorFactory(func(dir string, wcfg model.WatcherConfig, level string) (AgentExecutor, error) {
			return mockExec, nil
		})

		task := &model.Task{
			ID:         "task_001",
			CommandID:  "cmd_001",
			LeaseEpoch: 1,
			Attempts:   1,
		}

		err := dispatcher.DispatchTask(task, "worker1")
		if err != nil {
			t.Fatalf("DispatchTask failed: %v", err)
		}

		// Wait for async event delivery
		time.Sleep(50 * time.Millisecond)

		mu.Lock()
		count := len(taskStartedEvents)
		mu.Unlock()

		if count != 1 {
			t.Errorf("expected 1 task_started event, got %d", count)
		}

		mu.Lock()
		if count > 0 {
			e := taskStartedEvents[0]
			if e.Data["task_id"] != "task_001" {
				t.Errorf("expected task_id task_001, got %v", e.Data["task_id"])
			}
			if e.Data["command_id"] != "cmd_001" {
				t.Errorf("expected command_id cmd_001, got %v", e.Data["command_id"])
			}
		}
		mu.Unlock()
	})

	// Test that events do not block main processing
	t.Run("EventsAreNonBlocking", func(t *testing.T) {
		cfg := model.Config{
			Queue: model.QueueConfig{
				PriorityAgingSec: 60,
			},
			Watcher: model.WatcherConfig{},
			Logging: model.LoggingConfig{Level: "info"},
		}

		logger := log.New(&bytes.Buffer{}, "", 0)
		dispatcher := NewDispatcher(maestroDir, cfg, nil, logger, LogLevelInfo)
		dispatcher.SetEventBus(bus)

		// Use mock executor
		mockExec := &MockExecutor{
			executeResult: agent.ExecResult{Error: nil, Retryable: false},
		}
		dispatcher.SetExecutorFactory(func(dir string, wcfg model.WatcherConfig, level string) (AgentExecutor, error) {
			return mockExec, nil
		})

		task := &model.Task{
			ID:         "task_002",
			CommandID:  "cmd_002",
			LeaseEpoch: 1,
			Attempts:   1,
		}

		// Measure dispatch time
		start := time.Now()
		err := dispatcher.DispatchTask(task, "worker1")
		elapsed := time.Since(start)

		if err != nil {
			t.Fatalf("DispatchTask failed: %v", err)
		}

		// Dispatch should complete very quickly (< 10ms) even with event publishing
		if elapsed > 10*time.Millisecond {
			t.Errorf("DispatchTask took %v, expected < 10ms (events may be blocking)", elapsed)
		}
	})

	// Test that events do not cause failures when bus is nil
	t.Run("EventsAreOptionalWhenBusIsNil", func(t *testing.T) {
		cfg := model.Config{
			Queue: model.QueueConfig{
				PriorityAgingSec: 60,
			},
			Watcher: model.WatcherConfig{},
			Logging: model.LoggingConfig{Level: "info"},
		}

		logger := log.New(&bytes.Buffer{}, "", 0)
		dispatcher := NewDispatcher(maestroDir, cfg, nil, logger, LogLevelInfo)
		// Do NOT set event bus

		mockExec := &MockExecutor{
			executeResult: agent.ExecResult{Error: nil, Retryable: false},
		}
		dispatcher.SetExecutorFactory(func(dir string, wcfg model.WatcherConfig, level string) (AgentExecutor, error) {
			return mockExec, nil
		})

		task := &model.Task{
			ID:         "task_003",
			CommandID:  "cmd_003",
			LeaseEpoch: 1,
			Attempts:   1,
		}

		err := dispatcher.DispatchTask(task, "worker1")
		if err != nil {
			t.Fatalf("DispatchTask should succeed even without event bus: %v", err)
		}
	})
}

// TestEventHookPerformance verifies that event hooks maintain 100ms loop performance.
func TestEventHookPerformance(t *testing.T) {
	bus := events.NewBus(100)
	defer bus.Close()

	// Add subscriber
	unsub := bus.Subscribe(events.EventTaskStarted, func(e events.Event) {
		// Minimal processing
	})
	defer unsub()

	// Simulate 100ms loop with event publishing
	iterations := 100
	start := time.Now()

	for i := 0; i < iterations; i++ {
		bus.Publish(events.EventTaskStarted, map[string]interface{}{
			"task_id": "task_perf",
		})
		time.Sleep(1 * time.Millisecond) // Simulate minimal processing
	}

	elapsed := time.Since(start)
	avgPerIteration := elapsed / time.Duration(iterations)

	// Average iteration time should be close to 1ms (our sleep time)
	// If events are blocking, this would be much higher
	if avgPerIteration > 5*time.Millisecond {
		t.Errorf("average iteration time %v too high, events may be blocking", avgPerIteration)
	}
}

// MockExecutor for testing - implements AgentExecutor interface
type MockExecutor struct {
	executeResult agent.ExecResult
}

func (m *MockExecutor) Execute(req agent.ExecRequest) agent.ExecResult {
	return m.executeResult
}

func (m *MockExecutor) Close() error {
	return nil
}
