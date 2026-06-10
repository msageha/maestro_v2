// Package events provides an in-process event bus and audit logging for the maestro daemon.
package events

import (
	"context"
	"fmt"
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

// subscriber is a function that receives events.
type subscriber func(Event)

// subscriberChan wraps a subscriber channel with sync.Once to prevent double-close panics.
type subscriberChan struct {
	ch   chan Event
	once sync.Once
}

// safeClose closes the channel exactly once, regardless of how many times it is called.
func (s *subscriberChan) safeClose() {
	s.once.Do(func() { close(s.ch) })
}

// coalescedSubscriber receives coalesced notifications.
// Multiple rapid publishes are merged into a single callback invocation,
// guaranteeing at least one callback after any publish.
type coalescedSubscriber func()

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
// Returns a new slice (copy-on-write) so that concurrent readers iterating
// over the old slice are not affected by the mutation.
func removeSubscriber[T safeCloser](subs []T, target T) []T {
	for i, s := range subs {
		if any(s) == any(target) {
			newSubs := make([]T, 0, len(subs)-1)
			newSubs = append(newSubs, subs[:i]...)
			newSubs = append(newSubs, subs[i+1:]...)
			target.safeClose()
			return newSubs
		}
	}
	return subs
}

// criticalEventTypes lists event types that are always logged when dropped,
// regardless of the exponential backoff interval used for non-critical types.
var criticalEventTypes = map[EventType]bool{
	EventTaskStarted:     true,
	EventTaskCompleted:   true,
	EventPhaseTransition: true,
}

// Bus is a non-blocking event bus using Publish/Subscribe pattern.
// Events are delivered asynchronously via buffered channels.
// If a subscriber's channel is full, the event is dropped and counted.
//
// NOTE: Bus uses the standard log/slog package instead of DaemonLogger because
// it is a generic event infrastructure component in the events package.
// Introducing a daemon dependency would create a circular import
// (daemon → events → daemon).
type Bus struct {
	mu               sync.RWMutex
	closed           atomic.Bool
	subscribers      map[EventType][]*subscriberChan
	coalescedSubs    map[EventType][]*coalescedSub
	bufferSize       int
	wg               sync.WaitGroup
	closeWg          sync.WaitGroup // tracks the waitDone goroutine spawned by Close()
	droppedCount     atomic.Int64   // global total for O(1) DroppedCount()
	droppedByType    sync.Map       // EventType → *atomic.Int64
	ctx              context.Context
	cancel           context.CancelFunc
	activeGoroutines atomic.Int64
}

// NewBus creates a new event bus with the specified buffer size per subscriber.
// The provided context controls the bus lifetime; cancelling it is equivalent
// to calling Close and will stop all subscriber goroutines.
func NewBus(ctx context.Context, bufferSize int) *Bus {
	if bufferSize <= 0 {
		bufferSize = 100
	}
	ctx, cancel := context.WithCancel(ctx) //nolint:gosec // cancel is stored in Bus.cancel and called on Close
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
func (b *Bus) Subscribe(eventType EventType, fn subscriber) func() {
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
							slogc().Error("event_bus: subscriber panic", "event_type", string(event.Type), "panic", r, "stack", string(debug.Stack()))
						}
					}()
					fn(event)
				}()
			case <-b.ctx.Done():
				// Drain remaining buffered events so the channel becomes empty
				// and the goroutine exits promptly. Without this drain, the
				// goroutine would block on <-sub.ch until Close() closes the
				// channel, causing a goroutine leak if the subscriber is slow.
				for range sub.ch { //nolint:revive // intentional drain loop
				}
				return
			}
		}
	}()

	// Return unsubscribe function
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.closed.Load() {
			return
		}
		b.subscribers[eventType] = removeSubscriber(b.subscribers[eventType], sub)
	}
}

// SubscribeCoalesced registers a coalescing subscriber for a specific event type.
// Multiple rapid publishes are merged: the callback is invoked once per signal,
// guaranteeing at least one invocation after any publish. This prevents event
// loss for notification-style events like EventQueueWritten where the payload
// content does not matter—only the "something happened" signal.
// Returns an unsubscribe function.
func (b *Bus) SubscribeCoalesced(eventType EventType, fn coalescedSubscriber) func() {
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
							slogc().Error("event_bus: coalesced subscriber panic", "event_type", string(eventType), "panic", r, "stack", string(debug.Stack()))
						}
					}()
					fn()
				}()
			case <-b.ctx.Done():
				for range sub.sig { //nolint:revive // intentional drain loop
				}
				return
			}
		}
	}()

	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.closed.Load() {
			return
		}
		b.coalescedSubs[eventType] = removeSubscriber(b.coalescedSubs[eventType], sub)
	}
}

// Publish sends an event to all subscribers of the given type.
// Uses select with default to ensure non-blocking behavior.
// If a subscriber's channel is full, the event is dropped for that subscriber.
func (b *Bus) Publish(eventType EventType, data map[string]interface{}) {
	// Defensive recover: catches send-on-closed-channel panics as a safety net
	// beyond the double-check pattern, guarding against edge-case TOCTOU between
	// Publish and Close.
	defer func() {
		if r := recover(); r != nil {
			slogc().Warn("event_bus: recovered from panic in Publish", "event_type", string(eventType), "panic", r)
		}
	}()

	if b.closed.Load() {
		return
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	// Double-check under RLock to close TOCTOU race window.
	// Close() sets closed=true then acquires mu (exclusive). If Close()
	// completed between our first check and this RLock acquisition,
	// subscriber channels may already be closed — sending would panic.
	if b.closed.Load() {
		return
	}

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
			totalDropped := b.droppedCount.Add(1)
			typeCount := b.addDroppedByType(eventType)
			// Always log critical event drops; log others on first drop per type,
			// then at exponential intervals (powers of 2).
			critical := criticalEventTypes[eventType]
			if critical || typeCount == 1 || typeCount&(typeCount-1) == 0 {
				slogc().Warn("event_bus: event dropped",
					"event_type", string(eventType),
					"critical", critical,
					"type_dropped", typeCount,
					"total_dropped", totalDropped,
					"buffer_size", b.bufferSize)
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
// The sync.Map only stores *atomic.Int64 values (via LoadOrStore above), so the
// type assertion should always succeed. The !ok branch is a defensive fallback
// that logs the anomaly and returns a safe default rather than panicking.
func (b *Bus) addDroppedByType(eventType EventType) int64 {
	v, _ := b.droppedByType.LoadOrStore(eventType, &atomic.Int64{})
	counter, ok := v.(*atomic.Int64)
	if !ok {
		slogc().Error("event_bus: unexpected type in droppedByType map", "event_type", string(eventType), "actual_type", fmt.Sprintf("%T", v))
		return 0
	}
	return counter.Add(1)
}

// DroppedCount returns the total number of dropped events across all types.
func (b *Bus) DroppedCount() int64 {
	return b.droppedCount.Load()
}

// DroppedByType returns the number of dropped events for a specific event type.
func (b *Bus) DroppedByType(eventType EventType) int64 {
	v, loaded := b.droppedByType.Load(eventType)
	if !loaded {
		return 0
	}
	counter, ok := v.(*atomic.Int64)
	if !ok {
		return 0
	}
	return counter.Load()
}

// Close closes all subscriber channels, waits up to 5 seconds for goroutines to drain,
// and clears subscriptions. Returns an error if the timeout expires before all goroutines
// finish (remaining goroutines will finish asynchronously once their callbacks complete).
// Close is idempotent: calling it more than once is safe and subsequent calls return nil.
func (b *Bus) Close() error {
	if !b.closed.CompareAndSwap(false, true) {
		return nil
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
	// A single goroutine bridges sync.WaitGroup.Wait (which is not cancellable)
	// to a channel select. It is tracked by closeWg so that callers can verify
	// cleanup is complete even after Close returns on timeout.
	waitDone := make(chan struct{})
	b.closeWg.Add(1)
	go func() {
		defer b.closeWg.Done()
		b.wg.Wait()
		close(waitDone)
	}()

	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()

	select {
	case <-waitDone:
		return nil
	case <-timer.C:
		remaining := b.activeGoroutines.Load()
		return fmt.Errorf("event_bus: Close timed out waiting for %d subscriber goroutines", remaining)
	}
}
