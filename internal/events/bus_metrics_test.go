package events

import (
	"context"
	"sync"
	"testing"
)

func TestBus_DroppedCount(t *testing.T) {
	// Buffer size 1 so events get dropped quickly
	bus := NewBus(context.Background(), 1)
	defer func() {
		if err := bus.Close(); err != nil {
			t.Errorf("bus.Close: %v", err)
		}
	}()

	// Channels to coordinate: subscriber signals it started, then blocks until released.
	started := make(chan struct{})
	block := make(chan struct{})

	var once sync.Once
	unsub := bus.Subscribe(EventTaskStarted, func(e Event) {
		// The handler may run again for buffered events still pending when
		// close(block) unblocks the first call. Signal `started` only once:
		// the test's `<-started` receive happens exactly once, so a second
		// blocking send here would hang forever (and did, under -race,
		// before this fix — see bus.go's unsub(), which drops the
		// subscription from the map but does not close sub.ch, so the
		// goroutine keeps delivering already-buffered events until Close's
		// context cancellation is observed).
		once.Do(func() { started <- struct{}{} })
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
	defer func() {
		if err := bus.Close(); err != nil {
			t.Errorf("bus.Close: %v", err)
		}
	}()

	// Channels to coordinate: subscriber signals it started, then blocks until released.
	started := make(chan struct{})
	block := make(chan struct{})

	var once sync.Once
	unsub1 := bus.Subscribe(EventTaskStarted, func(e Event) {
		// See the comment in TestBus_DroppedCount: signal `started` only once.
		once.Do(func() { started <- struct{}{} })
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
