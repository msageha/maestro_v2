package reconcile

import (
	"fmt"

	"github.com/msageha/maestro_v2/internal/daemon/core"
	"github.com/msageha/maestro_v2/internal/lock"
	"github.com/msageha/maestro_v2/internal/model"
)

// Deps holds long-lived dependencies shared by all patterns.
type Deps struct {
	MaestroDir      string
	Config          model.Config
	LockMap         *lock.MutexMap
	DL              *core.DaemonLogger
	Clock           core.Clock
	ResultHandler   ResultNotifier
	ExecutorFactory core.ExecutorFactory
	CanComplete     core.CanCompleteFunc
}

// Validate checks that required dependencies are set.
// CanComplete is optional (R4 skips when nil), but the other fields are required.
func (d *Deps) Validate() error {
	if d.MaestroDir == "" {
		return fmt.Errorf("reconcile.Deps: MaestroDir is required")
	}
	if d.LockMap == nil {
		return fmt.Errorf("reconcile.Deps: LockMap is required")
	}
	if d.DL == nil {
		return fmt.Errorf("reconcile.Deps: DL (DaemonLogger) is required")
	}
	if d.Clock == nil {
		return fmt.Errorf("reconcile.Deps: Clock is required")
	}
	return nil
}

// ResultNotifier is the subset of ResultHandler needed by R5.
type ResultNotifier interface {
	WriteNotificationToOrchestratorQueue(resultID, commandID string, status model.Status) error
}
