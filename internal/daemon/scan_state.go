package daemon

import (
	"context"
	"time"

	"github.com/msageha/maestro_v2/internal/daemon/paneactivity"
	"github.com/msageha/maestro_v2/internal/model"
)

// fileState bundles a queue's data, file path, and dirty flag to eliminate
// the easy-to-mismatch {queue, path, dirty} triplets in scan phases.
type fileState[T any] struct {
	Data  T
	Path  string
	Dirty bool
}

// scanState holds all mutable state shared across Phase A step functions.
// It is passive data — behavior lives on QueueHandler methods.
type scanState struct {
	commands      fileState[model.CommandQueue]
	tasks         map[string]*taskQueueEntry
	taskDirty     map[string]bool
	notifications fileState[model.NotificationQueue]
	signals       fileState[model.PlannerSignalQueue]
	signalIndex   map[signalKey]struct{}
	work          deferredWork
	scanStart     time.Time
	// paneVerdicts caches the per-agent pane-activity verdict observed by
	// stepBlockedPaneTimeout earlier in the same scan tick. Downstream
	// lease-expiry handling MUST reuse this instead of re-observing: a
	// second ObserveVerdict in the same tick always lands within
	// minPrevAge of the first and returns a same-scan VerdictUncertain,
	// which the grace-extension path then extends — every lease expiry,
	// up to the 30-minute hard cap (E2E 2026-06-11: a worker whose claude
	// process had died was grace-extended for 30 minutes because each
	// expiry re-observed the pane just after the scan-tick observation).
	paneVerdicts map[string]paneactivity.Verdict
}

// forEachUntilCanceled iterates items, calling fn for each. If ctx is
// cancelled, iteration stops and ctx.Err() is returned.
func forEachUntilCanceled[T any](ctx context.Context, items []T, fn func(T)) error {
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		fn(item)
	}
	return nil
}
