package daemon

import (
	"github.com/msageha/maestro_v2/internal/events"
	"github.com/msageha/maestro_v2/internal/model"
)

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
	if d.eventBus == nil {
		return
	}

	// Subscribe to task started events
	unsub1 := d.eventBus.Subscribe(events.EventTaskStarted, func(e events.Event) {
		taskID, ok1 := e.Data["task_id"].(string)
		commandID, ok2 := e.Data["command_id"].(string)
		workerID, ok3 := e.Data["worker_id"].(string)

		if !ok1 || !ok2 || !ok3 {
			d.log(LogLevelWarn, "quality_gate_event_invalid type=task_started data=%v", e.Data)
			return
		}

		qgd := d.qualityGateDaemon
		if qgd == nil {
			return
		}
		qgd.EmitEvent(TaskStartEvent{
			TaskID:    taskID,
			CommandID: commandID,
			AgentID:   workerID,
			StartedAt: e.Timestamp,
		})
	})

	// Subscribe to task completed events
	unsub2 := d.eventBus.Subscribe(events.EventTaskCompleted, func(e events.Event) {
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

		qgd := d.qualityGateDaemon
		if qgd == nil {
			return
		}
		qgd.EmitEvent(TaskCompleteEvent{
			TaskID:      taskID,
			CommandID:   commandID,
			AgentID:     workerID,
			Status:      status,
			CompletedAt: e.Timestamp,
		})
	})

	// Subscribe to phase transition events
	unsub3 := d.eventBus.Subscribe(events.EventPhaseTransition, func(e events.Event) {
		phaseID, ok1 := e.Data["phase_id"].(string)
		commandID, ok2 := e.Data["command_id"].(string)
		oldStatus, ok3 := e.Data["old_status"].(string)
		newStatus, ok4 := e.Data["new_status"].(string)

		if !ok1 || !ok2 || !ok3 || !ok4 {
			d.log(LogLevelWarn, "quality_gate_event_invalid type=phase_transition data=%v", e.Data)
			return
		}

		qgd := d.qualityGateDaemon
		if qgd == nil {
			return
		}
		qgd.EmitEvent(PhaseTransitionEvent{
			PhaseID:        phaseID,
			CommandID:      commandID,
			OldStatus:      model.PhaseStatus(oldStatus),
			NewStatus:      model.PhaseStatus(newStatus),
			TransitionedAt: e.Timestamp,
		})
	})

	// Store unsubscribe functions for cleanup
	eb.eventUnsubscribers = []func(){unsub1, unsub2, unsub3}
}

// subscribeQueueWrittenEvents subscribes to EventQueueWritten to trigger scan
// directly via the event bus, bypassing fsnotify for daemon-originated writes.
func (eb *EventBridge) subscribeQueueWrittenEvents() {
	d := eb.d
	if d.eventBus == nil {
		return
	}
	unsub := d.eventBus.Subscribe(events.EventQueueWritten, func(e events.Event) {
		h := d.handler
		if h == nil || d.shuttingDown.Load() {
			return
		}
		file, _ := e.Data["file"].(string)
		h.debounceAndScan("event_bus:" + file)
	})
	eb.eventUnsubscribers = append(eb.eventUnsubscribers, unsub)
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

// --- Forwarding methods on Daemon for backward compatibility ---

func (d *Daemon) subscribeQualityGateEvents() {
	d.bridge.subscribeQualityGateEvents()
}

func (d *Daemon) subscribeQueueWrittenEvents() {
	d.bridge.subscribeQueueWrittenEvents()
}
