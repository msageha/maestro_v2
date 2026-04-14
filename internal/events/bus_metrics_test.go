package events

import (
	"context"
	"testing"
)

func TestBus_DroppedCount(t *testing.T) {
	// Buffer size 1 so events get dropped quickly
	bus := NewBus(context.Background(), 1)
	defer bus.Close()

	// Channels to coordinate: subscriber signals it started, then blocks until released.
	started := make(chan struct{})
	block := make(chan struct{})

	unsub := bus.Subscribe(EventTaskStarted, func(e Event) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-block
	})
	defer unsub()

	// Publish first event and wait until the subscriber goroutine picks it up.
	bus.Publish(EventTaskStarted, map[string]interface{}{"id": 0})
	<-started

	// Now the subscriber is blocked and the channel buffer (size 1) is empty.
	// Publish enough events to fill the buffer and cause drops.
	for i := 1; i <= 5; i++ {
		bus.Publish(EventTaskStarted, map[string]interface{}{"id": i})
	}

	if got := bus.DroppedCount(); got == 0 {
		t.Error("expected DroppedCount > 0 after publishing to full buffer")
	}

	// Unblock the subscriber so the goroutine can exit cleanly.
	close(block)
}

func TestBus_DroppedByType(t *testing.T) {
	bus := NewBus(context.Background(), 1)
	defer bus.Close()

	// Channels to coordinate: subscriber signals it started, then blocks until released.
	started := make(chan struct{})
	block := make(chan struct{})

	unsub1 := bus.Subscribe(EventTaskStarted, func(e Event) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-block
	})
	defer unsub1()

	// Fast subscriber on EventTaskCompleted (should not drop)
	unsub2 := bus.Subscribe(EventTaskCompleted, func(e Event) {})
	defer unsub2()

	// Publish first event and wait until the subscriber goroutine picks it up.
	bus.Publish(EventTaskStarted, map[string]interface{}{"id": 0})
	<-started

	// Now the subscriber is blocked and the channel buffer (size 1) is empty.
	// Fill up EventTaskStarted.
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

	// Unblock the subscriber so the goroutine can exit cleanly.
	close(block)
}
