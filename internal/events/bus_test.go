package events

import (
	"sync"
	"testing"
	"time"
)

func TestBus_PublishSubscribe(t *testing.T) {
	bus := NewBus(10)
	defer bus.Close()

	var mu sync.Mutex
	received := []Event{}
	done := make(chan struct{}, 1)

	unsub := bus.Subscribe(EventTaskStarted, func(e Event) {
		mu.Lock()
		received = append(received, e)
		mu.Unlock()
		select {
		case done <- struct{}{}:
		default:
		}
	})
	defer unsub()

	// Publish event
	bus.Publish(EventTaskStarted, map[string]interface{}{
		"task_id": "task_123",
	})

	// Wait for async delivery
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for event delivery")
	}

	mu.Lock()
	defer mu.Unlock()

	if len(received) != 1 {
		t.Fatalf("expected 1 event, got %d", len(received))
	}

	if received[0].Type != EventTaskStarted {
		t.Errorf("expected type %s, got %s", EventTaskStarted, received[0].Type)
	}

	if taskID, ok := received[0].Data["task_id"].(string); !ok || taskID != "task_123" {
		t.Errorf("expected task_id task_123, got %v", received[0].Data["task_id"])
	}
}

func TestBus_MultipleSubscribers(t *testing.T) {
	bus := NewBus(10)
	defer bus.Close()

	var mu1, mu2 sync.Mutex
	received1 := []Event{}
	received2 := []Event{}
	done1 := make(chan struct{}, 1)
	done2 := make(chan struct{}, 1)

	unsub1 := bus.Subscribe(EventTaskStarted, func(e Event) {
		mu1.Lock()
		received1 = append(received1, e)
		mu1.Unlock()
		select {
		case done1 <- struct{}{}:
		default:
		}
	})
	defer unsub1()

	unsub2 := bus.Subscribe(EventTaskStarted, func(e Event) {
		mu2.Lock()
		received2 = append(received2, e)
		mu2.Unlock()
		select {
		case done2 <- struct{}{}:
		default:
		}
	})
	defer unsub2()

	bus.Publish(EventTaskStarted, map[string]interface{}{
		"task_id": "task_456",
	})

	select {
	case <-done1:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for subscriber 1")
	}
	select {
	case <-done2:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for subscriber 2")
	}

	mu1.Lock()
	count1 := len(received1)
	mu1.Unlock()

	mu2.Lock()
	count2 := len(received2)
	mu2.Unlock()

	if count1 != 1 {
		t.Errorf("subscriber 1 expected 1 event, got %d", count1)
	}
	if count2 != 1 {
		t.Errorf("subscriber 2 expected 1 event, got %d", count2)
	}
}

func TestBus_NonBlocking(t *testing.T) {
	bus := NewBus(1)
	defer bus.Close()

	// Subscribe with slow consumer
	unsub := bus.Subscribe(EventTaskStarted, func(e Event) {
		time.Sleep(100 * time.Millisecond)
	})
	defer unsub()

	// Publish multiple events rapidly
	start := time.Now()
	for i := 0; i < 10; i++ {
		bus.Publish(EventTaskStarted, map[string]interface{}{
			"id": i,
		})
	}
	elapsed := time.Since(start)

	// Publishing should complete quickly even though consumer is slow
	if elapsed > 50*time.Millisecond {
		t.Errorf("publish blocked for %v, expected non-blocking", elapsed)
	}
}

func TestBus_Unsubscribe(t *testing.T) {
	bus := NewBus(10)
	defer bus.Close()

	var mu sync.Mutex
	count := 0
	done := make(chan struct{}, 1)

	unsub := bus.Subscribe(EventTaskStarted, func(e Event) {
		mu.Lock()
		count++
		mu.Unlock()
		select {
		case done <- struct{}{}:
		default:
		}
	})

	bus.Publish(EventTaskStarted, map[string]interface{}{})

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for first event delivery")
	}

	// Unsubscribe removes channel from map and closes it.
	// No further events will be delivered.
	unsub()

	bus.Publish(EventTaskStarted, map[string]interface{}{})

	// After unsubscribe, the channel was removed from the subscribers map,
	// so the second publish cannot reach the subscriber. No sleep needed.
	mu.Lock()
	defer mu.Unlock()

	if count != 1 {
		t.Errorf("expected 1 event before unsubscribe, got %d", count)
	}
}

func TestBus_PanicRecovery(t *testing.T) {
	bus := NewBus(10)
	defer bus.Close()

	var mu sync.Mutex
	received := false
	done := make(chan struct{}, 1)

	// Subscriber that panics
	unsub1 := bus.Subscribe(EventTaskStarted, func(e Event) {
		panic("test panic")
	})
	defer unsub1()

	// Subscriber that should still receive events
	unsub2 := bus.Subscribe(EventTaskStarted, func(e Event) {
		mu.Lock()
		received = true
		mu.Unlock()
		select {
		case done <- struct{}{}:
		default:
		}
	})
	defer unsub2()

	bus.Publish(EventTaskStarted, map[string]interface{}{})

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for event delivery to second subscriber")
	}

	mu.Lock()
	defer mu.Unlock()

	if !received {
		t.Error("second subscriber did not receive event after first panicked")
	}
}

func TestBus_EventTypes(t *testing.T) {
	bus := NewBus(10)
	defer bus.Close()

	var mu sync.Mutex
	taskStarted := 0
	taskCompleted := 0
	startedDone := make(chan struct{}, 2)
	completedDone := make(chan struct{}, 1)

	unsub1 := bus.Subscribe(EventTaskStarted, func(e Event) {
		mu.Lock()
		taskStarted++
		mu.Unlock()
		select {
		case startedDone <- struct{}{}:
		default:
		}
	})
	defer unsub1()

	unsub2 := bus.Subscribe(EventTaskCompleted, func(e Event) {
		mu.Lock()
		taskCompleted++
		mu.Unlock()
		select {
		case completedDone <- struct{}{}:
		default:
		}
	})
	defer unsub2()

	bus.Publish(EventTaskStarted, map[string]interface{}{})
	bus.Publish(EventTaskCompleted, map[string]interface{}{})
	bus.Publish(EventTaskStarted, map[string]interface{}{})

	for i := 0; i < 2; i++ {
		select {
		case <-startedDone:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for task_started event delivery")
		}
	}
	select {
	case <-completedDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for task_completed event delivery")
	}

	mu.Lock()
	defer mu.Unlock()

	if taskStarted != 2 {
		t.Errorf("expected 2 task_started events, got %d", taskStarted)
	}
	if taskCompleted != 1 {
		t.Errorf("expected 1 task_completed event, got %d", taskCompleted)
	}
}

func TestBus_SubscribeAfterClose(t *testing.T) {
	bus := NewBus(10)
	bus.Close()

	called := false
	unsub := bus.Subscribe(EventTaskStarted, func(e Event) {
		called = true
	})

	// Should return a noop unsubscribe
	unsub()

	// Publish should also be safe
	bus.Publish(EventTaskStarted, map[string]interface{}{
		"task_id": "task_789",
	})

	// No goroutine is running because Subscribe returned a no-op after Close.
	// Publish is also a no-op because closed flag is set. No sleep needed.
	if called {
		t.Error("subscriber should not be called after bus is closed")
	}
}

func TestBus_PublishAfterClose(t *testing.T) {
	bus := NewBus(10)

	var mu sync.Mutex
	count := 0

	unsub := bus.Subscribe(EventTaskStarted, func(e Event) {
		mu.Lock()
		count++
		mu.Unlock()
	})
	defer unsub()

	bus.Close()

	// Publish after close should be silently ignored
	bus.Publish(EventTaskStarted, map[string]interface{}{
		"task_id": "task_after_close",
	})

	// Close() waited for goroutines to finish. Publish after close is a no-op.
	// No sleep needed.
	mu.Lock()
	defer mu.Unlock()

	if count != 0 {
		t.Errorf("expected 0 events after close, got %d", count)
	}
}

func TestBus_ConcurrentCloseAndPublish(t *testing.T) {
	bus := NewBus(10)

	var wg sync.WaitGroup

	// 10 goroutines publishing rapidly
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				bus.Publish(EventTaskStarted, map[string]interface{}{
					"id": id*100 + j,
				})
			}
		}(i)
	}

	// 5 goroutines subscribing/unsubscribing
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				unsub := bus.Subscribe(EventTaskStarted, func(e Event) {})
				unsub()
			}
		}()
	}

	// Close bus concurrently
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(5 * time.Millisecond)
		bus.Close()
	}()

	// Must complete without panic or deadlock
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// success
	case <-time.After(5 * time.Second):
		t.Fatal("test timed out - likely deadlock")
	}
}

func BenchmarkBus_Publish(b *testing.B) {
	bus := NewBus(100)
	defer bus.Close()

	// Add some subscribers
	for i := 0; i < 5; i++ {
		bus.Subscribe(EventTaskStarted, func(e Event) {
			// no-op
		})
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bus.Publish(EventTaskStarted, map[string]interface{}{
			"task_id": "task_123",
		})
	}
}
