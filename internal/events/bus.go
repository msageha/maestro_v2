// Package events provides an in-process event bus and audit logging for the maestro daemon.
package events

import (
	"context"
	"log"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
)

// EventType represents the type of event being published.
type EventType string

const (
	// EventTaskStarted is published when a task is dispatched to a worker.
	EventTaskStarted EventType = "task_started"
	// EventTaskCompleted is published when a task result is processed.
	EventTaskCompleted EventType = "task_completed"
	// EventPhaseTransition is published when a phase status changes.
	EventPhaseTransition EventType = "phase_transition"
	// EventQueueWritten is published when the daemon writes to a queue file via UDS handler,
	// or when a plan operation (submit/complete/add_retry_task) writes queue files.
	// Data keys: "file" (base filename, optional), "source" ("uds" or "plan_*"), "type" (write type).
	EventQueueWritten EventType = "queue_written"
)

// Event represents a system event.
type Event struct {
	Type      EventType
	Timestamp time.Time
	Data      map[string]interface{}
}

// Subscriber is a function that receives events.
type Subscriber func(Event)

// subscriberChan wraps a subscriber channel with sync.Once to prevent double-close panics.
type subscriberChan struct {
	ch   chan Event
	once sync.Once
}

// safeClose closes the channel exactly once, regardless of how many times it is called.
func (s *subscriberChan) safeClose() {
	s.once.Do(func() { close(s.ch) })
}

// CoalescedSubscriber receives coalesced notifications.
// Multiple rapid publishes are merged into a single callback invocation,
// guaranteeing at least one callback after any publish.
type CoalescedSubscriber func()

// coalescedSub wraps a coalescing signal channel with sync.Once for safe close.
type coalescedSub struct {
	sig  chan struct{}
	once sync.Once
}

func (c *coalescedSub) safeClose() {
	c.once.Do(func() { close(c.sig) })
}

// safeCloser is implemented by subscriber types that support idempotent close.
type safeCloser interface {
	safeClose()
}

// removeSubscriber removes a specific subscriber from a slice and closes it.
// Returns the updated slice.
func removeSubscriber[T safeCloser](subs []T, target T) []T {
	for i, s := range subs {
		if any(s) == any(target) {
			last := len(subs) - 1
			subs[i] = subs[last]
			var zero T
			subs[last] = zero
			target.safeClose()
			return subs[:last]
		}
	}
	return subs
}

// Bus is a non-blocking event bus using Publish/Subscribe pattern.
// Events are delivered asynchronously via buffered channels.
// If a subscriber's channel is full, the event is dropped and counted.
type Bus struct {
	mu               sync.RWMutex
	closed           atomic.Bool
	subscribers      map[EventType][]*subscriberChan
	coalescedSubs    map[EventType][]*coalescedSub
	bufferSize       int
	wg               sync.WaitGroup
	droppedCount     atomic.Int64   // global total for O(1) DroppedCount()
	droppedByType    sync.Map       // EventType → *atomic.Int64
	ctx              context.Context
	cancel           context.CancelFunc
	activeGoroutines atomic.Int64
}

// NewBus creates a new event bus with the specified buffer size per subscriber.
func NewBus(bufferSize int) *Bus {
	if bufferSize <= 0 {
		bufferSize = 100
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Bus{
		subscribers:   make(map[EventType][]*subscriberChan),
		coalescedSubs: make(map[EventType][]*coalescedSub),
		bufferSize:    bufferSize,
		ctx:           ctx,
		cancel:        cancel,
	}
}

// Subscribe registers a subscriber for a specific event type.
// The subscriber function is called asynchronously in a goroutine.
// Returns an unsubscribe function.
func (b *Bus) Subscribe(eventType EventType, fn Subscriber) func() {
	if b.closed.Load() {
		return func() {}
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	// Double-check under lock to close TOCTOU race window.
	// Close() sets closed=true then acquires mu. If Close() completed
	// between our first check and this lock acquisition, we must bail out.
	if b.closed.Load() {
		return func() {}
	}

	sub := &subscriberChan{ch: make(chan Event, b.bufferSize)}
	b.subscribers[eventType] = append(b.subscribers[eventType], sub)

	// Start goroutine to deliver events to subscriber
	b.wg.Add(1)
	b.activeGoroutines.Add(1)
	go func() {
		defer b.wg.Done()
		defer b.activeGoroutines.Add(-1)
		for {
			select {
			case event, ok := <-sub.ch:
				if !ok {
					return
				}
				// Wrap in anonymous function to recover from panics in subscriber
				func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("ERROR event_bus: subscriber panic for event %s: %v\n%s",
								event.Type, r, debug.Stack())
						}
					}()
					fn(event)
				}()
			case <-b.ctx.Done():
				// Drain remaining buffered events from the channel to ensure
				// the goroutine exits promptly after context cancellation.
				for range sub.ch {
				}
				return
			}
		}
	}()

	// Return unsubscribe function
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.subscribers[eventType] = removeSubscriber(b.subscribers[eventType], sub)
	}
}

// SubscribeCoalesced registers a coalescing subscriber for a specific event type.
// Multiple rapid publishes are merged: the callback is invoked once per signal,
// guaranteeing at least one invocation after any publish. This prevents event
// loss for notification-style events like EventQueueWritten where the payload
// content does not matter—only the "something happened" signal.
// Returns an unsubscribe function.
func (b *Bus) SubscribeCoalesced(eventType EventType, fn CoalescedSubscriber) func() {
	if b.closed.Load() {
		return func() {}
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed.Load() {
		return func() {}
	}

	sub := &coalescedSub{sig: make(chan struct{}, 1)}
	b.coalescedSubs[eventType] = append(b.coalescedSubs[eventType], sub)

	b.wg.Add(1)
	b.activeGoroutines.Add(1)
	go func() {
		defer b.wg.Done()
		defer b.activeGoroutines.Add(-1)
		for {
			select {
			case _, ok := <-sub.sig:
				if !ok {
					return
				}
				func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("ERROR event_bus: coalesced subscriber panic for event %s: %v\n%s",
								eventType, r, debug.Stack())
						}
					}()
					fn()
				}()
			case <-b.ctx.Done():
				for range sub.sig {
				}
				return
			}
		}
	}()

	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.coalescedSubs[eventType] = removeSubscriber(b.coalescedSubs[eventType], sub)
	}
}

// Publish sends an event to all subscribers of the given type.
// Uses select with default to ensure non-blocking behavior.
// If a subscriber's channel is full, the event is dropped for that subscriber.
func (b *Bus) Publish(eventType EventType, data map[string]interface{}) {
	if b.closed.Load() {
		return
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	event := Event{
		Type:      eventType,
		Timestamp: time.Now().UTC(),
		Data:      data,
	}

	subscribers := b.subscribers[eventType]
	for _, sub := range subscribers {
		// Non-blocking send using select with default
		select {
		case sub.ch <- event:
			// Event delivered successfully
		default:
			// Channel full, drop event and record
			b.droppedCount.Add(1)
			typeCount := b.addDroppedByType(eventType)
			// Log on first drop per type, then sample every 100 drops to avoid log flooding
			if typeCount == 1 || typeCount%100 == 0 {
				log.Printf("WARN event_bus: event dropped for type %s (type dropped: %d)", eventType, typeCount)
			}
		}
	}

	// Signal coalesced subscribers (never drops — at-least-once delivery)
	for _, csub := range b.coalescedSubs[eventType] {
		select {
		case csub.sig <- struct{}{}:
		default:
			// Already signaled, coalesced — no drop
		}
	}
}

// addDroppedByType atomically increments the per-type dropped counter and returns the new value.
func (b *Bus) addDroppedByType(eventType EventType) int64 {
	v, _ := b.droppedByType.LoadOrStore(eventType, &atomic.Int64{})
	return v.(*atomic.Int64).Add(1)
}

// DroppedCount returns the total number of events dropped due to full subscriber channels.
func (b *Bus) DroppedCount() int64 {
	return b.droppedCount.Load()
}

// DroppedCountByType returns the number of events dropped for a specific event type.
func (b *Bus) DroppedCountByType(eventType EventType) int64 {
	v, ok := b.droppedByType.Load(eventType)
	if !ok {
		return 0
	}
	return v.(*atomic.Int64).Load()
}

// Close closes all subscriber channels, waits for goroutines to drain, and clears subscriptions.
// Close is idempotent: calling it more than once is safe and subsequent calls are no-ops.
func (b *Bus) Close() {
	if !b.closed.CompareAndSwap(false, true) {
		return
	}

	// Cancel context to signal subscriber goroutines to stop
	b.cancel()

	b.mu.Lock()
	for eventType, subs := range b.subscribers {
		for _, sub := range subs {
			sub.safeClose()
		}
		delete(b.subscribers, eventType)
	}
	for eventType, subs := range b.coalescedSubs {
		for _, sub := range subs {
			sub.safeClose()
		}
		delete(b.coalescedSubs, eventType)
	}
	b.mu.Unlock()

	// Wait for subscriber goroutines to finish with timeout.
	// Use a timer instead of spawning a goroutine to avoid leaking
	// a waiter goroutine when the timeout fires.
	waitDone := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(waitDone)
	}()

	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()

	select {
	case <-waitDone:
	case <-timer.C:
		remaining := b.activeGoroutines.Load()
		buf := make([]byte, 64*1024)
		n := runtime.Stack(buf, true)
		log.Printf("WARN event_bus: Close timed out waiting for %d subscriber goroutines, dumping stacks:\n%s",
			remaining, string(buf[:n]))
		// Wait for the waiter goroutine to finish so it does not leak.
		// At this point all channels are closed, so wg.Wait() will
		// complete once goroutines drain their channels.
		<-waitDone
	}
}
