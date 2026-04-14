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

// subscribeQualityGateEvents subscribes the QualityGateDaemon to EventBus events.
// It bridges the generic event bus to the quality gate daemon's typed event channel.
// Uses safe type assertions with logging for dropped events.
func (eb *EventBridge) subscribeQualityGateEvents() {
	d := eb.d
	if d.qualityGateDaemon == nil {
		return
	}

	// Subscribe to task started events
	unsub1 := d.eventBus.Subscribe(events.EventTaskStarted, func(e events.Event) {
		defer func() {
			if r := recover(); r != nil {
				d.log(LogLevelError, "panic in event_bridge callback type=task_started: %v\n%s", r, debug.Stack())
				d.Shutdown()
			}
		}()
		taskID, ok1 := e.Data["task_id"].(string)
		commandID, ok2 := e.Data["command_id"].(string)
		workerID, ok3 := e.Data["worker_id"].(string)

		if !ok1 || !ok2 || !ok3 {
			d.log(LogLevelWarn, "quality_gate_event_invalid type=task_started data=%v", e.Data)
			return
		}

		eb.runWithTimeout("task_started", func(_ context.Context) {
			d.qualityGateDaemon.EmitEvent(TaskStartEvent{
				TaskID:    taskID,
				CommandID: commandID,
				AgentID:   workerID,
				StartedAt: e.Timestamp,
			})
		})
	})

	// Subscribe to task completed events
	unsub2 := d.eventBus.Subscribe(events.EventTaskCompleted, func(e events.Event) {
		defer func() {
			if r := recover(); r != nil {
				d.log(LogLevelError, "panic in event_bridge callback type=task_completed: %v\n%s", r, debug.Stack())
				d.Shutdown()
			}
		}()
		taskID, ok1 := e.Data["task_id"].(string)
		commandID, ok2 := e.Data["command_id"].(string)
		workerID, ok3 := e.Data["worker_id"].(string)

		if !ok1 || !ok2 || !ok3 {
			d.log(LogLevelWarn, "quality_gate_event_invalid type=task_completed data=%v", e.Data)
			return
		}

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

		eb.runWithTimeout("task_completed", func(_ context.Context) {
			d.qualityGateDaemon.EmitEvent(TaskCompleteEvent{
				TaskID:      taskID,
				CommandID:   commandID,
				AgentID:     workerID,
				Status:      status,
				CompletedAt: e.Timestamp,
			})
		})
	})

	// Subscribe to phase transition events
	unsub3 := d.eventBus.Subscribe(events.EventPhaseTransition, func(e events.Event) {
		defer func() {
			if r := recover(); r != nil {
				d.log(LogLevelError, "panic in event_bridge callback type=phase_transition: %v\n%s", r, debug.Stack())
				d.Shutdown()
			}
		}()
		phaseID, ok1 := e.Data["phase_id"].(string)
		commandID, ok2 := e.Data["command_id"].(string)
		oldStatus, ok3 := e.Data["old_status"].(string)
		newStatus, ok4 := e.Data["new_status"].(string)

		if !ok1 || !ok2 || !ok3 || !ok4 {
			d.log(LogLevelWarn, "quality_gate_event_invalid type=phase_transition data=%v", e.Data)
			return
		}

		eb.runWithTimeout("phase_transition", func(_ context.Context) {
			d.qualityGateDaemon.EmitEvent(PhaseTransitionEvent{
				PhaseID:        phaseID,
				CommandID:      commandID,
				OldStatus:      model.PhaseStatus(oldStatus),
				NewStatus:      model.PhaseStatus(newStatus),
				TransitionedAt: e.Timestamp,
			})
		})
	})

	// Store unsubscribe functions for cleanup
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
