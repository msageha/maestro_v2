package daemon

import (
	"context"
	"runtime/debug"
	"time"

	"github.com/msageha/maestro_v2/internal/events"
	"github.com/msageha/maestro_v2/internal/model"
)

// eventBridgeCallbackTimeout is the maximum duration for a single event bridge
// callback execution. Prevents a hung callback from blocking the event bus.
const eventBridgeCallbackTimeout = 10 * time.Second

// EventBridge bridges the generic event bus to typed daemon component events.
// It holds a back-pointer to Daemon for access to shared state.
type EventBridge struct {
	d                  *Daemon
	eventUnsubscribers []func()
}

// subscribeWithRecovery subscribes to an event type with panic recovery.
// The handler is wrapped with a deferred recover that logs the panic and
// triggers a daemon shutdown, matching the original per-callback behavior.
func (eb *EventBridge) subscribeWithRecovery(eventType events.EventType, callbackName string, handler func(events.Event)) func() {
	return eb.d.eventBus.Subscribe(eventType, func(e events.Event) {
		defer func() {
			if r := recover(); r != nil {
				eb.d.log(LogLevelError, "panic in event_bridge callback type=%s: %v\n%s", callbackName, r, debug.Stack())
				eb.d.Shutdown()
			}
		}()
		handler(e)
	})
}

// extractStringFields extracts string values from an event data map for the given keys.
// Returns the values in key order and false if any key is missing or not a string.
func extractStringFields(data map[string]interface{}, keys []string) ([]string, bool) {
	vals := make([]string, len(keys))
	for i, k := range keys {
		v, ok := data[k].(string)
		if !ok {
			return nil, false
		}
		vals[i] = v
	}
	return vals, true
}

// subscribeQualityGateTyped subscribes to an event type, extracts string fields,
// validates them, and runs the handler with timeout. Reduces boilerplate across
// quality gate event subscriptions.
func (eb *EventBridge) subscribeQualityGateTyped(
	eventType events.EventType,
	name string,
	keys []string,
	handler func(vals []string, e events.Event),
) func() {
	d := eb.d
	return eb.subscribeWithRecovery(eventType, name, func(e events.Event) {
		vals, ok := extractStringFields(e.Data, keys)
		if !ok {
			d.log(LogLevelWarn, "quality_gate_event_invalid type=%s data=%v", name, e.Data)
			return
		}
		eb.runWithTimeout(name, func(_ context.Context) {
			handler(vals, e)
		})
	})
}

// subscribeQualityGateEvents subscribes the QualityGateDaemon to EventBus events.
// It bridges the generic event bus to the quality gate daemon's typed event channel.
// Uses safe type assertions with logging for dropped events.
func (eb *EventBridge) subscribeQualityGateEvents() {
	d := eb.d
	if d.qualityGateDaemon == nil {
		return
	}

	unsub1 := eb.subscribeQualityGateTyped(events.EventTaskStarted, "task_started",
		[]string{"task_id", "command_id", "worker_id"},
		func(vals []string, e events.Event) {
			d.qualityGateDaemon.EmitEvent(TaskStartEvent{
				TaskID:    vals[0],
				CommandID: vals[1],
				AgentID:   vals[2],
				StartedAt: e.Timestamp,
			})
		})

	unsub2 := eb.subscribeQualityGateTyped(events.EventTaskCompleted, "task_completed",
		[]string{"task_id", "command_id", "worker_id"},
		func(vals []string, e events.Event) {
			// Status can be either model.Status or string
			var status model.Status
			if s, ok := e.Data["status"].(model.Status); ok {
				status = s
			} else if s, ok := e.Data["status"].(string); ok {
				status = model.Status(s)
			} else {
				d.log(LogLevelWarn, "quality_gate_event_invalid type=task_completed status=%v", e.Data["status"])
				return
			}
			d.qualityGateDaemon.EmitEvent(TaskCompleteEvent{
				TaskID:      vals[0],
				CommandID:   vals[1],
				AgentID:     vals[2],
				Status:      status,
				CompletedAt: e.Timestamp,
			})
		})

	unsub3 := eb.subscribeQualityGateTyped(events.EventPhaseTransition, "phase_transition",
		[]string{"phase_id", "command_id", "old_status", "new_status"},
		func(vals []string, e events.Event) {
			d.qualityGateDaemon.EmitEvent(PhaseTransitionEvent{
				PhaseID:        vals[0],
				CommandID:      vals[1],
				OldStatus:      model.PhaseStatus(vals[2]),
				NewStatus:      model.PhaseStatus(vals[3]),
				TransitionedAt: e.Timestamp,
			})
		})

	eb.eventUnsubscribers = []func(){unsub1, unsub2, unsub3}
}

// subscribeQueueWrittenEvents subscribes to EventQueueWritten using a coalescing
// channel to trigger scans. This guarantees at least one scan after any queue write,
// even during bursts, without dropping notifications.
func (eb *EventBridge) subscribeQueueWrittenEvents() {
	d := eb.d
	unsub := d.eventBus.SubscribeCoalesced(events.EventQueueWritten, func() {
		defer func() {
			if r := recover(); r != nil {
				d.log(LogLevelError, "panic in event_bridge callback type=queue_written: %v\n%s", r, debug.Stack())
				d.Shutdown()
			}
		}()
		if d.handler == nil || d.shuttingDown.Load() {
			return
		}
		d.handler.scanExecutor.debounce.Trigger("event_bus:queue_written")
	})
	eb.eventUnsubscribers = append(eb.eventUnsubscribers, unsub)
}

// runWithTimeout executes fn with a timeout. Returns true if fn completed within
// the deadline, false if it timed out. A context derived from the daemon's
// context with the timeout applied is passed to fn, allowing it to observe
// cancellation and exit promptly. Panics inside fn are recovered, logged,
// and trigger a daemon shutdown (matching the original callback behavior).
func (eb *EventBridge) runWithTimeout(callbackName string, fn func(ctx context.Context)) bool {
	ctx, cancel := context.WithTimeout(eb.d.ctx, eventBridgeCallbackTimeout)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				eb.d.log(LogLevelError, "panic in event_bridge callback type=%s: %v\n%s", callbackName, r, debug.Stack())
				eb.d.Shutdown()
			}
			close(done)
		}()
		fn(ctx)
	}()

	select {
	case <-done:
		return true
	case <-ctx.Done():
		eb.d.log(LogLevelError, "event_bridge callback timeout type=%s timeout=%s", callbackName, eventBridgeCallbackTimeout)
		return false
	}
}

// unsubscribeAll unsubscribes all event bus subscriptions managed by this bridge.
func (eb *EventBridge) unsubscribeAll() {
	for _, unsub := range eb.eventUnsubscribers {
		if unsub != nil {
			unsub()
		}
	}
	eb.eventUnsubscribers = nil
}
