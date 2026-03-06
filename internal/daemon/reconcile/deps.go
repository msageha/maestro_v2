package reconcile

import (
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

// ResultNotifier is the subset of ResultHandler needed by R5.
type ResultNotifier interface {
	WriteNotificationToOrchestratorQueue(resultID, commandID string, status model.Status) error
}
