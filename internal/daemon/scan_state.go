package daemon

import (
	"context"
	"time"

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
