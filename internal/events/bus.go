package events

import (
	"context"
	"log"
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
)

// Event represents a system event.
type Event struct {
	Type      EventType
	Timestamp time.Time
	Data      map[string]interface{}
}

// Subscriber is a function that receives events.
type Subscriber func(Event)

// Bus is a non-blocking event bus using Publish/Subscribe pattern.
// Events are delivered asynchronously via buffered channels.
// If a subscriber's channel is full, the event is dropped and counted.
type Bus struct {
	mu               sync.RWMutex
	closed           atomic.Bool
	subscribers      map[EventType][]chan Event
	bufferSize       int
	wg               sync.WaitGroup
	droppedCount     atomic.Int64
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
		subscribers: make(map[EventType][]chan Event),
		bufferSize:  bufferSize,
		ctx:         ctx,
		cancel:      cancel,
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

	ch := make(chan Event, b.bufferSize)
	b.subscribers[eventType] = append(b.subscribers[eventType], ch)

	// Start goroutine to deliver events to subscriber
	b.wg.Add(1)
	b.activeGoroutines.Add(1)
	go func() {
		defer b.wg.Done()
		defer b.activeGoroutines.Add(-1)
		for {
			select {
			case event, ok := <-ch:
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
				return
			}
		}
	}()

	// Return unsubscribe function
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()

		subs := b.subscribers[eventType]
		for i, subCh := range subs {
			if subCh == ch {
				// Remove subscriber from slice
				b.subscribers[eventType] = append(subs[:i], subs[i+1:]...)
				close(ch)
				break
			}
		}
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
	for _, ch := range subscribers {
		// Non-blocking send using select with default
		select {
		case ch <- event:
			// Event delivered successfully
		default:
			// Channel full, drop event and record
			count := b.droppedCount.Add(1)
			// Log on first drop, then sample every 100 drops to avoid log flooding
			if count == 1 || count%100 == 0 {
				log.Printf("WARN event_bus: event dropped for type %s (total dropped: %d)", eventType, count)
			}
		}
	}
}

// DroppedCount returns the total number of events dropped due to full subscriber channels.
func (b *Bus) DroppedCount() int64 {
	return b.droppedCount.Load()
}

// Close closes all subscriber channels, waits for goroutines to drain, and clears subscriptions.
func (b *Bus) Close() {
	b.closed.Store(true)

	// Cancel context to signal subscriber goroutines to stop
	b.cancel()

	b.mu.Lock()
	for eventType, subs := range b.subscribers {
		for _, ch := range subs {
			close(ch)
		}
		delete(b.subscribers, eventType)
	}
	b.mu.Unlock()

	// Wait for subscriber goroutines to finish with timeout
	done := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		remaining := b.activeGoroutines.Load()
		log.Printf("WARN event_bus: Close timed out waiting for %d subscriber goroutines", remaining)
	}
}
