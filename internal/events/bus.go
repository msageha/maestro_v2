package events

import (
	"log"
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
// If a subscriber's channel is full, the event is dropped silently.
type Bus struct {
	mu          sync.RWMutex
	closed      atomic.Bool
	subscribers map[EventType][]chan Event
	bufferSize  int
	wg          sync.WaitGroup
}

// NewBus creates a new event bus with the specified buffer size per subscriber.
func NewBus(bufferSize int) *Bus {
	if bufferSize <= 0 {
		bufferSize = 100
	}
	return &Bus{
		subscribers: make(map[EventType][]chan Event),
		bufferSize:  bufferSize,
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
	go func() {
		defer b.wg.Done()
		for event := range ch {
			// Wrap in anonymous function to recover from panics in subscriber
			func() {
				defer func() {
					if r := recover(); r != nil {
						// Silently recover from subscriber panics to prevent bus disruption
					}
				}()
				fn(event)
			}()
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
			// Channel full, drop event silently to prevent blocking
		}
	}
}

// Close closes all subscriber channels, waits for goroutines to drain, and clears subscriptions.
func (b *Bus) Close() {
	b.closed.Store(true)

	b.mu.Lock()
	for eventType, subs := range b.subscribers {
		for _, ch := range subs {
			close(ch)
		}
		delete(b.subscribers, eventType)
	}
	b.mu.Unlock()

	// Wait for subscriber goroutines to finish with timeout to prevent deadlock
	done := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		log.Printf("WARN event_bus: Close timed out waiting for subscriber goroutines")
	}
}
