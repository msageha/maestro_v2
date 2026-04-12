package events

import (
	"context"
	"testing"
	"time"
)

func TestBus_DroppedCount(t *testing.T) {
	// Buffer size 1 so events get dropped quickly
	bus := NewBus(context.Background(), 1)
	defer bus.Close()

	// Slow subscriber that blocks the channel
	unsub := bus.Subscribe(EventTaskStarted, func(e Event) {
		time.Sleep(500 * time.Millisecond)
	})
	defer unsub()

	// Wait for subscriber goroutine to start and pick up first event
	bus.Publish(EventTaskStarted, map[string]interface{}{"id": 0})
	time.Sleep(20 * time.Millisecond)

	// Publish enough events to fill the buffer and cause drops
	for i := 1; i <= 5; i++ {
		bus.Publish(EventTaskStarted, map[string]interface{}{"id": i})
	}

	if got := bus.DroppedCount(); got == 0 {
		t.Error("expected DroppedCount > 0 after publishing to full buffer")
	}
}

func TestBus_DroppedByType(t *testing.T) {
	bus := NewBus(context.Background(), 1)
	defer bus.Close()

	// Slow subscriber on EventTaskStarted
	unsub1 := bus.Subscribe(EventTaskStarted, func(e Event) {
		time.Sleep(500 * time.Millisecond)
	})
	defer unsub1()

	// Fast subscriber on EventTaskCompleted (should not drop)
	unsub2 := bus.Subscribe(EventTaskCompleted, func(e Event) {})
	defer unsub2()

	// Fill up EventTaskStarted
	bus.Publish(EventTaskStarted, map[string]interface{}{"id": 0})
	time.Sleep(20 * time.Millisecond)
	for i := 1; i <= 5; i++ {
		bus.Publish(EventTaskStarted, map[string]interface{}{"id": i})
	}

	// Publish to EventTaskCompleted (fast, should not drop)
	bus.Publish(EventTaskCompleted, map[string]interface{}{"id": 1})

	startedDrops := bus.DroppedByType(EventTaskStarted)
	completedDrops := bus.DroppedByType(EventTaskCompleted)

	if startedDrops == 0 {
		t.Error("expected DroppedByType(EventTaskStarted) > 0")
	}
	if completedDrops != 0 {
		t.Errorf("expected DroppedByType(EventTaskCompleted) == 0, got %d", completedDrops)
	}

	// Unknown type should return 0
	if got := bus.DroppedByType(EventType("nonexistent")); got != 0 {
		t.Errorf("expected DroppedByType for unknown type == 0, got %d", got)
	}
}
